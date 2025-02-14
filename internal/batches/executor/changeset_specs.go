package executor

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/sourcegraph/go-diff/diff"
	batcheslib "github.com/sourcegraph/sourcegraph/lib/batches"
	"github.com/sourcegraph/sourcegraph/lib/batches/template"

	"github.com/sourcegraph/src-cli/internal/batches"
	"github.com/sourcegraph/src-cli/internal/batches/util"
)

var errOptionalPublishedUnsupported = batcheslib.NewValidationError(errors.New(`This Sourcegraph version requires the "published" field to be specified in the batch spec; upgrade to version 3.30.0 or later to be able to omit the published field and control publication from the UI.`))

func createChangesetSpecs(task *Task, result executionResult, features batches.FeatureFlags) ([]*batcheslib.ChangesetSpec, error) {
	tmplCtx := &template.ChangesetTemplateContext{
		BatchChangeAttributes: *task.BatchChangeAttributes,
		Steps: template.StepsContext{
			Changes: result.ChangedFiles,
			Path:    result.Path,
		},
		Outputs:    result.Outputs,
		Repository: util.NewTemplatingRepo(task.Repository.Name, task.Repository.FileMatches),
	}

	var authorName string
	var authorEmail string

	if task.Template.Commit.Author == nil {
		if features.IncludeAutoAuthorDetails {
			// user did not provide author info, so use defaults
			authorName = "Sourcegraph"
			authorEmail = "batch-changes@sourcegraph.com"
		}
	} else {
		var err error
		authorName, err = template.RenderChangesetTemplateField("authorName", task.Template.Commit.Author.Name, tmplCtx)
		if err != nil {
			return nil, err
		}
		authorEmail, err = template.RenderChangesetTemplateField("authorEmail", task.Template.Commit.Author.Email, tmplCtx)
		if err != nil {
			return nil, err
		}
	}

	title, err := template.RenderChangesetTemplateField("title", task.Template.Title, tmplCtx)
	if err != nil {
		return nil, err
	}

	body, err := template.RenderChangesetTemplateField("body", task.Template.Body, tmplCtx)
	if err != nil {
		return nil, err
	}

	message, err := template.RenderChangesetTemplateField("message", task.Template.Commit.Message, tmplCtx)
	if err != nil {
		return nil, err
	}

	// TODO: As a next step, we should extend the ChangesetTemplateContext to also include
	// TransformChanges.Group and then change validateGroups and groupFileDiffs to, for each group,
	// render the branch name *before* grouping the diffs.
	defaultBranch, err := template.RenderChangesetTemplateField("branch", task.Template.Branch, tmplCtx)
	if err != nil {
		return nil, err
	}

	newSpec := func(branch, diff string) (*batcheslib.ChangesetSpec, error) {
		var published interface{} = nil
		if task.Template.Published != nil {
			published = task.Template.Published.ValueWithSuffix(task.Repository.Name, branch)

			// Backward compatibility: before optional published fields were
			// allowed, ValueWithSuffix() would fall back to false, not nil. We
			// need to replicate this behaviour here.
			if published == nil && !features.AllowOptionalPublished {
				published = false
			}
		} else if !features.AllowOptionalPublished {
			return nil, errOptionalPublishedUnsupported
		}

		return &batcheslib.ChangesetSpec{
			BaseRepository: task.Repository.ID,

			BaseRef:        task.Repository.BaseRef(),
			BaseRev:        task.Repository.Rev(),
			HeadRepository: task.Repository.ID,
			HeadRef:        util.EnsureRefPrefix(branch),
			Title:          title,
			Body:           body,
			Commits: []batcheslib.GitCommitDescription{
				{
					Message:     message,
					AuthorName:  authorName,
					AuthorEmail: authorEmail,
					Diff:        diff,
				},
			},
			Published: batcheslib.PublishedValue{Val: published},
		}, nil
	}

	var specs []*batcheslib.ChangesetSpec

	groups := groupsForRepository(task.Repository.Name, task.TransformChanges)
	if len(groups) != 0 {
		err := validateGroups(task.Repository.Name, task.Template.Branch, groups)
		if err != nil {
			return specs, err
		}

		// TODO: Regarding 'defaultBranch', see comment above
		diffsByBranch, err := groupFileDiffs(result.Diff, defaultBranch, groups)
		if err != nil {
			return specs, errors.Wrap(err, "grouping diffs failed")
		}

		for branch, diff := range diffsByBranch {
			spec, err := newSpec(branch, diff)
			if err != nil {
				return nil, err
			}
			specs = append(specs, spec)
		}
	} else {
		spec, err := newSpec(defaultBranch, result.Diff)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}

	return specs, nil
}

func groupsForRepository(repoName string, transform *batcheslib.TransformChanges) []batcheslib.Group {
	groups := []batcheslib.Group{}

	if transform == nil {
		return groups
	}

	for _, g := range transform.Group {
		if g.Repository != "" {
			if g.Repository == repoName {
				groups = append(groups, g)
			}
		} else {
			groups = append(groups, g)
		}
	}

	return groups
}

func validateGroups(repoName, defaultBranch string, groups []batcheslib.Group) error {
	uniqueBranches := make(map[string]struct{}, len(groups))

	for _, g := range groups {
		if _, ok := uniqueBranches[g.Branch]; ok {
			return batcheslib.NewValidationError(fmt.Errorf("transformChanges would lead to multiple changesets in repository %s to have the same branch %q", repoName, g.Branch))
		} else {
			uniqueBranches[g.Branch] = struct{}{}
		}

		if g.Branch == defaultBranch {
			return batcheslib.NewValidationError(fmt.Errorf("transformChanges group branch for repository %s is the same as branch %q in changesetTemplate", repoName, defaultBranch))
		}
	}

	return nil
}

func groupFileDiffs(completeDiff, defaultBranch string, groups []batcheslib.Group) (map[string]string, error) {
	fileDiffs, err := diff.ParseMultiFileDiff([]byte(completeDiff))
	if err != nil {
		return nil, err
	}

	// Housekeeping: we setup these two datastructures so we can
	// - access the group.Branch by the directory for which they should be used
	// - check against the given directories, in order.
	branchesByDirectory := make(map[string]string, len(groups))
	dirs := make([]string, len(branchesByDirectory))
	for _, g := range groups {
		branchesByDirectory[g.Directory] = g.Branch
		dirs = append(dirs, g.Directory)
	}

	byBranch := make(map[string][]*diff.FileDiff, len(groups))
	byBranch[defaultBranch] = []*diff.FileDiff{}

	// For each file diff...
	for _, f := range fileDiffs {
		name := f.NewName
		if name == "/dev/null" {
			name = f.OrigName
		}

		// .. we check whether it matches one of the given directories in the
		// group transformations, with the last match winning:
		var matchingDir string
		for _, d := range dirs {
			if strings.Contains(name, d) {
				matchingDir = d
			}
		}

		// If the diff didn't match a rule, it goes into the default branch and
		// the default changeset.
		if matchingDir == "" {
			byBranch[defaultBranch] = append(byBranch[defaultBranch], f)
			continue
		}

		// If it *did* match a directory, we look up which branch we should use:
		branch, ok := branchesByDirectory[matchingDir]
		if !ok {
			panic("this should not happen: " + matchingDir)
		}

		byBranch[branch] = append(byBranch[branch], f)
	}

	finalDiffsByBranch := make(map[string]string, len(byBranch))
	for branch, diffs := range byBranch {
		printed, err := diff.PrintMultiFileDiff(diffs)
		if err != nil {
			return nil, errors.Wrap(err, "printing multi file diff failed")
		}
		finalDiffsByBranch[branch] = string(printed)
	}
	return finalDiffsByBranch, nil
}
