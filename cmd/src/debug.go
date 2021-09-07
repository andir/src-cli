package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/sourcegraph/src-cli/internal/exec"
)

type podList struct {
	Items []struct {
		Metadata struct {
			Name string
		}
		Spec struct {
			Containers []struct {
				Name string
			}
		}
	}
}

type archiveFile struct {
	name string
	data []byte
	err  error
}

// Init debug flag on src build
func init() {
	flagSet := flag.NewFlagSet("debug", flag.ExitOnError)

	usageFunc := func() {

		fmt.Fprintf(flag.CommandLine.Output(), `'src debug' gathers and bundles debug data from a Sourcegraph deployment.

USAGE
  src [-v] debug -d=<deployment type> [-out=debug.zip]
`)
	}

	// store value passed to flags
	var (
		deployment = flagSet.String("d", "", "deployment type")
		base       = flagSet.String("out", "debug.zip", "The name of the output zip archive")
	)

	handler := func(args []string) error {
		if err := flagSet.Parse(args); err != nil {
			return err
		}

		//validate out flag
		if *base == "" {
			return fmt.Errorf("empty -out flag")
		}
		// declare basedir for archive file structure
		var baseDir string
		if strings.HasSuffix(*base, ".zip") == false {
			baseDir = *base
			*base = *base + ".zip"
		} else {
			baseDir = strings.TrimSuffix(*base, ".zip")
		}

		// open pipe to output file
		out, err := os.OpenFile(*base, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0666)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}

		if err := setOpenFileLimits(64000); err != nil {
			return fmt.Errorf("failed to set open file limits: %w", err)
		}

		// open zip writer
		defer out.Close()
		zw := zip.NewWriter(out)
		defer zw.Close()

		ctx := context.Background()
		// TODO write functions for sourcegraph server and docker-compose instances
		switch *deployment {
		case "serv":
			if err := archiveDocker(ctx, zw, *verbose, baseDir); err != nil {
				return fmt.Errorf("archiveDocker failed with err: %w", err)
			}
		case "comp":
			if err := archiveDocker(ctx, zw, *verbose, baseDir); err != nil {
				return fmt.Errorf("archiveDocker failed with err: %w", err)
			}
		case "kube":
			if err := archiveKube(ctx, zw, *verbose, baseDir); err != nil {
				return fmt.Errorf("archiveKube failed with err: %w", err)
			}
		default:
			return fmt.Errorf("must declare -d=<deployment type>, as serv, comp, or kube")
		}

		return nil
	}

	// Register the command.
	commands = append(commands, &command{
		aliases:   []string{"debug-dump"},
		flagSet:   flagSet,
		handler:   handler,
		usageFunc: usageFunc,
	})
}

// setOpenFileLimits increases the limit of open files to the given number. This is needed
// when doings lots of concurrent network requests which establish open sockets.
func setOpenFileLimits(n uint64) error {
	var rlimit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit)
	if err != nil {
		return err
	}

	rlimit.Max = n
	rlimit.Cur = n

	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit)
}

/*
Kubernetes functions
TODO: handle namespaces
*/

// Run kubectl functions concurrently and archive results to zip file
func archiveKube(ctx context.Context, zw *zip.Writer, verbose bool, baseDir string) error {
	// Create a context with a cancel function that we call when returning
	// from archiveKube. This ensures we close all pending go-routines when returning
	// early because of an error.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pods, err := getPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pods: %w", err)
	}

	if verbose {
		log.Printf("getting kubectl data for %d pods...\n", len(pods.Items))
	}

	// setup channel for slice of archive function outputs
	ch := make(chan *archiveFile)
	wg := sync.WaitGroup{}

	// create goroutine to get kubectl events
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch <- getEvents(ctx, baseDir)
	}()

	// create goroutine to get persistent volumes
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch <- getPV(ctx, baseDir)
	}()

	// create goroutine to get persistent volumes claim
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch <- getPVC(ctx, baseDir)
	}()

	// start goroutine to run kubectl logs for each pod's container's
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			wg.Add(1)
			go func(pod, container string) {
				defer wg.Done()
				ch <- getContainerLog(ctx, pod, container, baseDir)
			}(pod.Metadata.Name, container.Name)
		}
	}

	// start goroutine to run kubectl logs --previous for each pod's container's
	// won't write to zip on err, only passes bytes to channel if err not nil
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			wg.Add(1)
			go func(pod, container string) {
				defer wg.Done()
				f := getPastContainerLog(ctx, pod, container, baseDir)
				if f.err == nil {
					ch <- f
				}
			}(pod.Metadata.Name, container.Name)
		}
	}

	// start goroutine for each pod to run kubectl describe pod
	for _, pod := range pods.Items {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			ch <- getDescribe(ctx, pod, baseDir)
		}(pod.Metadata.Name)
	}

	// start goroutine for each pod to run kubectl get pod <pod> -o yaml
	for _, pod := range pods.Items {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			ch <- getManifest(ctx, pod, baseDir)
		}(pod.Metadata.Name)
	}

	// close channel when wait group goroutines have completed
	go func() {
		wg.Wait()
		close(ch)
	}()

	// write to archive all the outputs from kubectl call functions passed to buffer channel
	for f := range ch {
		if f.err != nil {
			return fmt.Errorf("aborting due to error on %s: %v\noutput: %s", f.name, f.err, f.data)
		}

		if verbose {
			log.Printf("archiving file %q with %d bytes", f.name, len(f.data))
		}

		zf, err := zw.Create(f.name)
		if err != nil {
			return fmt.Errorf("failed to create %s: %w", f.name, err)
		}

		_, err = zf.Write(f.data)
		if err != nil {
			return fmt.Errorf("failed to write to %s: %w", f.name, err)
		}
	}

	return nil
}

func getPods(ctx context.Context) (podList, error) {
	// Declare buffer type var for kubectl pipe
	var podsBuff bytes.Buffer

	// Get all pod names as json
	getPods := exec.CommandContext(ctx, "kubectl", "get", "pods", "-l", "deploy=sourcegraph", "-o=json")
	getPods.Stdout = &podsBuff
	getPods.Stderr = os.Stderr
	err := getPods.Run()

	//Declare struct to format decode from podList
	var pods podList

	//Decode json from podList
	if err := json.NewDecoder(&podsBuff).Decode(&pods); err != nil {
		fmt.Errorf("failed to unmarshall get pods json: %w", err)
	}

	return pods, err
}

func getEvents(ctx context.Context, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/events.txt"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "get", "events", "--all-namespaces").CombinedOutput()
	return f
}

func getPV(ctx context.Context, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/persistent-volumes.txt"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "get", "pv").CombinedOutput()
	return f
}

func getPVC(ctx context.Context, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/persistent-volume-claims.txt"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "get", "pvc").CombinedOutput()
	return f
}

// get kubectl logs for pod containers
func getContainerLog(ctx context.Context, podName, containerName, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/pods/" + podName + "/" + containerName + ".log"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "logs", podName, "-c", containerName).CombinedOutput()
	return f
}

// get kubectl logs for past container
func getPastContainerLog(ctx context.Context, podName, containerName, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/pods/" + podName + "/" + "prev-" + containerName + ".log"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "logs", "--previous", podName, "-c", containerName).CombinedOutput()
	return f
}

func getDescribe(ctx context.Context, podName, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/pods/" + podName + "/describe-" + podName + ".txt"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "describe", "pod", podName).CombinedOutput()
	return f
}

func getManifest(ctx context.Context, podName, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/kubectl/pods/" + podName + "/manifest-" + podName + ".yaml"}
	f.data, f.err = exec.CommandContext(ctx, "kubectl", "get", "pod", podName, "-o", "yaml").CombinedOutput()
	return f
}

/*
Docker functions

*/

func archiveDocker(ctx context.Context, zw *zip.Writer, verbose bool, baseDir string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	containers, err := getContainers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get docker containers: %w", err)
	}

	if verbose {
		log.Printf("getting docker data for %d containers...\n", len(containers))
	}

	// setup channel for slice of archive function outputs
	ch := make(chan *archiveFile)
	wg := sync.WaitGroup{}

	// start goroutine to run docker container stats --no-stream
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch <- getStats(ctx, baseDir)
	}()

	// start goroutine to run docker container logs <container>
	for _, container := range containers {
		wg.Add(1)
		go func(container string) {
			defer wg.Done()
			ch <- getLog(ctx, container, baseDir)
		}(container)
	}

	// start goroutine to run docker container inspect <container>
	for _, container := range containers {
		wg.Add(1)
		go func(container string) {
			defer wg.Done()
			ch <- getInspect(ctx, container, baseDir)
		}(container)
	}

	// close channel when wait group goroutines have completed
	go func() {
		wg.Wait()
		close(ch)
	}()

	for f := range ch {
		if f.err != nil {
			return fmt.Errorf("aborting due to error on %s: %v\noutput: %s", f.name, f.err, f.data)
		}

		if verbose {
			log.Printf("archiving file %q with %d bytes", f.name, len(f.data))
		}

		zf, err := zw.Create(f.name)
		if err != nil {
			return fmt.Errorf("failed to create %s: %w", f.name, err)
		}

		_, err = zf.Write(f.data)
		if err != nil {
			return fmt.Errorf("failed to write to %s: %w", f.name, err)
		}
	}

	return nil
}

func getContainers(ctx context.Context) ([]string, error) {
	c, err := exec.CommandContext(ctx, "docker", "container", "ls", "--format", "{{.Names}}").Output()
	if err != nil {
		fmt.Errorf("failed to get container names with error: %w", err)
	}
	s := string(c)
	containers := strings.Split(strings.TrimSpace(s), "\n")
	fmt.Println(containers)
	return containers, err
}

func getLog(ctx context.Context, container, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/docker/containers/" + container + "/" + container + ".log"}
	f.data, f.err = exec.CommandContext(ctx, "docker", "container", "logs", container).CombinedOutput()
	return f
}

func getInspect(ctx context.Context, container, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/docker/containers/" + container + "/inspect-" + container + ".txt"}
	f.data, f.err = exec.CommandContext(ctx, "docker", "container", "inspect", container).CombinedOutput()
	return f
}

func getStats(ctx context.Context, baseDir string) *archiveFile {
	f := &archiveFile{name: baseDir + "/docker/stats.txt"}
	f.data, f.err = exec.CommandContext(ctx, "docker", "container", "stats", "--no-stream").CombinedOutput()
	return f
}

/*
Graveyard
-----------
*/

//if err := archiveEvents(zw, baseDir); err != nil {
//	return fmt.Errorf("running archiveEvents failed: %w", err)
//}
//if err := archivePV(zw, baseDir); err != nil {
//	return fmt.Errorf("running archivePV failed: %w", err)
//}
//if err := archivePVC(zw, baseDir); err != nil {
//	return fmt.Errorf("running archivePV failed: %w", err)
//}
//if err := archiveLogs(zw, pods, baseDir); err != nil {
//	return fmt.Errorf("running archiveLogs failed: %w", err)
//}
//if err := archiveDescribes(zw, pods, baseDir); err != nil {
//	return fmt.Errorf("running archiveDescribes failed: %w", err)
//}
//if err := archiveManifests(zw, pods, baseDir); err != nil {
//	return fmt.Errorf("running archiveManifests failed: %w", err)
//}

//// gets current pod logs and logs from past containers
//func getLogs(pods podList, baseDir string) (fs []archiveFile) {
//
//	// run kubectl logs and write to archive, accounts for containers in pod
//	for _, pod := range pods.Items {
//		fmt.Println("Archiving logs: ", pod.Metadata.Name, "Containers:", pod.Spec.Containers)
//		for _, container := range pod.Spec.Containers {
//			logs, err := zw.Create(baseDir + "/kubectl/pods/" + pod.Metadata.Name + "/" + container.Name + ".log")
//			if err != nil {
//				return fmt.Errorf("failed to create podLogs.txt: %w", err)
//			}
//
//			getLogs := exec.CommandContext(ctx, "kubectl", "logs", pod.Metadata.Name, "-c", container.Name)
//			getLogs.Stdout = logs
//			getLogs.Stderr = os.Stderr
//
//			if err := getLogs.Run(); err != nil {
//				return fmt.Errorf("running kubectl get logs failed: %w", err)
//			}
//		}
//	}
//
//	// run kubectl logs --previous and write to archive if return not err
//	for _, pod := range pods.Items {
//		for _, container := range pod.Spec.Containers {
//			getPrevLogs := exec.CommandContext(ctx, "kubectl", "logs", "--previous", pod.Metadata.Name, "-c", container.Name)
//			if err := getPrevLogs.Run(); err == nil {
//				fmt.Println("Archiving previous logs: ", pod.Metadata.Name, "Containers: ", pod.Spec.Containers)
//				prev, err := zw.Create(baseDir + "/kubectl/pods/" + pod.Metadata.Name + "/" + "prev-" + container.Name + ".log")
//				getPrevLogs.Stdout = prev
//				if err != nil {
//					return fmt.Errorf("failed to create podLogs.txt: %w", err)
//				}
//			}
//		}
//	}
//
//	return nil
//}

//func archiveManifests(zw *zip.Writer, pods podList, baseDir string) error {
//	for _, pod := range pods.Items {
//		manifests, err := zw.Create(baseDir + "/kubectl/pods/" + pod.Metadata.Name + "/manifest-" + pod.Metadata.Name + ".yaml")
//		if err != nil {
//			return fmt.Errorf("failed to create manifest.yaml: %w", err)
//		}
//
//		getManifest := exec.CommandContext(ctx, "kubectl", "get", "pod", pod.Metadata.Name, "-o", "yaml")
//		getManifest.Stdout = manifests
//		getManifest.Stderr = os.Stderr
//
//		if err := getManifest.Run(); err != nil {
//			fmt.Errorf("failed to get pod yaml: %w", err)
//		}
//	}
//	return nil
//}
