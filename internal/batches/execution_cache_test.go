	"io/ioutil"
		testTempDir, err := ioutil.TempDir("", "execution-disk-cache-test-*")
	if err := ioutil.WriteFile(path, raw, 0600); err != nil {
	if err := ioutil.WriteFile(path, []byte(diff), 0600); err != nil {