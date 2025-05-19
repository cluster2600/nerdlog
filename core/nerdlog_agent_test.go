package core

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dimonomid/nerdlog/util/sysloggen"
	"github.com/juju/errors"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v2"
)

const agentTestOutputRoot = "/tmp/nerdlog_agent_test_output"
const agentTestCaseYamlFname = "test_case.yaml"

type AgentTestCaseYaml struct {
	Descr    string           `yaml:"descr"`
	Logfiles TestCaseLogfiles `yaml:"logfiles"`

	// CurYear and CurMonth specify today's date. If not specified, 1970-01 will
	// be used. This matters for inferring the log's year (because traditional
	// syslog timestamp format doesn't include year).
	CurYear  int `yaml:"cur_year"`
	CurMonth int `yaml:"cur_month"`

	// Env can contain extra environment variables to set, in the format
	// VARIABLE=VALUE. It'll be passed to cmd.Env directly.
	Env []string `yaml:"env"`

	Args []string `yaml:"args"`
}

type TestCaseLogfiles struct {
	Kind LogfilesKind `yaml:"kind"`

	// Dir is only relevant for LogfilesKindAllFromDir
	Dir string `yaml:"dir"`

	// JournalctlDataFile is only relevant for LogfilesKindJournalctl
	JournalctlDataFile string `yaml:"journalctl_data_file"`
}

type LogfilesKind string

const (
	LogfilesKindAllFromDir LogfilesKind = "all_from_dir"
	LogfilesKindJournalctl LogfilesKind = "journalctl"
)

var AllLogfilesKinds = map[LogfilesKind]struct{}{
	LogfilesKindAllFromDir: {},
	LogfilesKindJournalctl: {},
}

func TestNerdlogAgent(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to get caller info")
	}

	// Get directory of the current file
	parentDir := filepath.Dir(filename)
	testCasesDir := filepath.Join(parentDir, "core_testdata", "test_cases_agent")
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	repoRoot := filepath.Dir(filepath.Dir(filename))

	if err := os.MkdirAll(agentTestOutputRoot, 0755); err != nil {
		t.Fatalf("unable to create agent test output root dir %s: %s", agentTestOutputRoot, err.Error())
	}

	testCaseDirs, err := getTestCaseDirs(testCasesDir, agentTestCaseYamlFname)
	if err != nil {
		panic(err)
	}

	for _, testCaseDir := range testCaseDirs {
		t.Run(testCaseDir, func(t *testing.T) {
			if err := runAgentTestCase(t, nerdlogAgentShFname, testCasesDir, repoRoot, testCaseDir); err != nil {
				t.Fatalf("running agent test case %s: %s", testCaseDir, err.Error())
			}
		})
	}
}

func runAgentTestCase(t *testing.T, nerdlogAgentShFname, testCasesDir, repoRoot, testName string) error {
	testCaseDir := filepath.Join(testCasesDir, testName)
	testCaseDescrFname := filepath.Join(testCaseDir, agentTestCaseYamlFname)

	testOutputDir := filepath.Join(agentTestOutputRoot, testName)
	if err := os.MkdirAll(testOutputDir, 0755); err != nil {
		return errors.Annotatef(err, "unable to create test output dir %s", testOutputDir)
	}

	data, err := os.ReadFile(testCaseDescrFname)
	if err != nil {
		return errors.Annotatef(err, "reading yaml test case descriptor %s", testCaseDescrFname)
	}

	var tc AgentTestCaseYaml
	if err := yaml.Unmarshal(data, &tc); err != nil {
		return errors.Annotatef(err, "unmarshaling yaml from %s", testCaseDescrFname)
	}

	resolved, err := resolveLogfiles(testCaseDir, &tc.Logfiles)
	if err != nil {
		return errors.Annotatef(err, "resolving logfiles")
	}

	provisioned, err := provisionLogFiles(resolved, testOutputDir, repoRoot)
	if err != nil {
		return errors.Annotatef(err, "provisioning logfiles")
	}

	indexFname := filepath.Join(testOutputDir, "nerdlog_agent_index")

	os.Remove(indexFname)

	cmdArgs := []string{
		nerdlogAgentShFname,
		"query",
		"--logfile-last", provisioned.logfileLast,
		"--logfile-prev", provisioned.logfilePrev,
		"--index-file", indexFname,
	}

	if provisioned.logfileLast == "journalctl" {
		// Specify time format (normally LStreamClient autodetects the time format
		// and provides these).
		cmdArgs = append(
			cmdArgs,
			"--awktime-month", "substr($0, 6, 2)",
			"--awktime-year", "substr($0, 1, 4)",
			"--awktime-day", "substr($0, 9, 2)",
			"--awktime-hhmm", "substr($0, 12, 5)",
			"--awktime-minute-key", "substr($0, 6, 11)",
		)
	}

	cmdArgs = append(cmdArgs, tc.Args...)

	// Do the full run, with the provided initial index (which in most cases
	// means, without any index)
	if err := runNerdlogAgent(t, &tc, cmdArgs, testCaseDir, provisioned.extraEnv, testName, testNerdlogAgentParams{
		checkStderr: true,
	}); err != nil {
		return errors.Trace(err)
	}

	// For journalctl tests, there is no index, and therefore nothing else to do.
	if provisioned.logfileLast == "journalctl" {
		return nil
	}

	// For log files tests, we rerun the test multiple times after removing some
	// latest lines from the index, expecting it to index up and to produce the same
	// result.

	// If asked to skip these repetitions with indexing up, we're done.
	//
	// There are at least two valid reasons to skip:
	//
	// - When we're running "make update-test-expectations", because for that we
	//   need to capture stderr after the main test (without repetitions), since
	//   stderr will be different otherwise
	// - To run tests much faster (these repetitions are the slowest part of tests)
	if os.Getenv("NERDLOG_AGENT_TEST_SKIP_INDEX_UP") != "" {
		return nil
	}

	// indexReduceStep specifies how many lines we remove from the index at every
	// step here. For most tests, it's 25.
	indexReduceStep := 25

	// For the tests of handling decreased timestamps, reduce the index by a single
	// line, to make sure that some corner case doesn't slip through.
	if strings.Contains(testName, "decreased") {
		indexReduceStep = 1
	}

	// Backup the resulting fully-built index
	indexFullFname := filepath.Join(testOutputDir, "nerdlog_agent_index_full")
	if err := copyFile(indexFname, indexFullFname); err != nil {
		return errors.Annotatef(err, "backing up index as full index: from %s to %s", indexFname, indexFullFname)
	}

	// Now, keep running the same query with smaller index: on every iteration,
	// we'll remove one more line from the index end, and expect the same stdout
	// (not stderr though, this one will be different).

	numLines, err := getNumLines(indexFullFname)
	if err != nil {
		return errors.Annotatef(err, "getting numer of lines in %s", indexFullFname)
	}

	// We can only remove the "idx" lines from the index.
	minLineno, err := getLastNonMatchingLine(indexFullFname, "idx")
	if err != nil {
		return errors.Annotatef(err, "getLastNonMatchingLine")
	}

	// minLineno points to the line containing the last non-"idx" entry, but
	// we actually need at least one "idx" entry in the index file for it to work,
	// so we increment it.
	minLineno += 1

	for keepLines := numLines - 1; ; keepLines -= indexReduceStep {
		// If we step too much below the min, use the min (and we'll break below).
		if keepLines < minLineno {
			keepLines = minLineno
		}

		_, err := copyUpToNumLines(indexFullFname, indexFname, keepLines)
		if err != nil {
			return errors.Annotatef(
				err, "copying up to %d lines from %s to %s",
				keepLines, indexFullFname, indexFname,
			)
		}

		t.Run(fmt.Sprintf("keep_%d_lines", keepLines), func(t *testing.T) {
			if err := runNerdlogAgent(t, &tc, cmdArgs, testCaseDir, provisioned.extraEnv, testName, testNerdlogAgentParams{
				// When changing the index, stderr would change too.
				checkStderr: false,
			}); err != nil {
				t.Fatalf("error: %s", err.Error())
			}
		})

		if keepLines <= minLineno {
			break
		}
	}

	// TODO: the stats lines in stdout (these starting from "s:") are printed in
	// arbitrary order because they come from a hashmap, so simply comparing the
	// output is a bad idea. Gotta do some post-processing, like sorting these "s:"
	// lines, before comparing them.

	return nil
}

type testNerdlogAgentParams struct {
	checkStderr bool
}

func runNerdlogAgent(
	t *testing.T, tc *AgentTestCaseYaml, bashArgs []string, testCaseDir string,
	extraEnv []string,
	testName string,
	params testNerdlogAgentParams,
) error {
	assertArgs := []interface{}{"test case %s", testName}

	testOutputDir := filepath.Join(agentTestOutputRoot, testName)

	stdoutFname := filepath.Join(testOutputDir, "nerdlog_agent_stdout")
	stderrFname := filepath.Join(testOutputDir, "nerdlog_agent_stderr")
	os.Remove(stdoutFname)
	os.Remove(stderrFname)

	stdoutFile, err := os.Create(stdoutFname)
	defer stdoutFile.Close()

	stderrFile, err := os.Create(stderrFname)
	defer stderrFile.Close()

	cmd := exec.Command("/usr/bin/env", append([]string{"bash"}, bashArgs...)...)

	curYear := tc.CurYear
	if curYear == 0 {
		curYear = 1970
	}

	curMonth := tc.CurMonth
	if curMonth == 0 {
		curMonth = 1
	}

	agentEnv := []string{
		"TZ=UTC",
		fmt.Sprintf("CUR_YEAR=%d", curYear),
		fmt.Sprintf("CUR_MONTH=%d", curMonth),
	}

	agentEnv = append(agentEnv, tc.Env...)
	agentEnv = append(agentEnv, extraEnv...)

	cmd.Env = append(
		os.Environ(),
		agentEnv...,
	)
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	fmt.Printf("Running %+v\n", bashArgs)
	if err := cmd.Run(); err != nil {
		return errors.Annotatef(err, "running nerdlog query command %+v", bashArgs)
	}

	wantStdout, err := os.ReadFile(filepath.Join(testCaseDir, "want_stdout"))
	if err != nil {
		return errors.Annotatef(err, "reading want_stdout")
	}

	wantStderr, err := os.ReadFile(filepath.Join(testCaseDir, "want_stderr"))
	if err != nil {
		return errors.Annotatef(err, "reading want_stderr")
	}

	gotStdout, err := os.ReadFile(stdoutFname)
	if err != nil {
		return errors.Annotatef(err, "reading %s", stdoutFname)
	}

	gotStderr, err := os.ReadFile(stderrFname)
	if err != nil {
		return errors.Annotatef(err, "reading %s", stderrFname)
	}

	assert.Equal(t, string(wantStdout), string(gotStdout), assertArgs...)

	if params.checkStderr {
		assert.Equal(t, string(wantStderr), string(gotStderr), assertArgs...)
	}

	return nil
}

func runNerdlogAgentForBenchmark(
	bashArgs []string,
) error {
	cmd := exec.Command("/usr/bin/env", append([]string{"bash"}, bashArgs...)...)

	cmd.Env = append(os.Environ(), "TZ=UTC")

	if err := cmd.Run(); err != nil {
		return errors.Annotatef(err, "running nerdlog query command %+v", bashArgs)
	}

	return nil
}

type resolvedLogFiles struct {
	// If files is not empty, we need to use these files.
	files []string

	// If journalctlDataFile is not empty, we need to use that file
	// as the data for mocked journalctl.
	journalctlDataFile string
}

func resolveLogfiles(
	testCaseDir string, logfilesDescr *TestCaseLogfiles,
) (*resolvedLogFiles, error) {
	switch logfilesDescr.Kind {
	case LogfilesKindAllFromDir:
		logfilesDir := filepath.Join(testCaseDir, logfilesDescr.Dir)

		entries, err := os.ReadDir(logfilesDir)
		if err != nil {
			return nil, errors.Annotatef(err, "reading logfiles dir %q", logfilesDir)
		}

		var files []string
		for _, entry := range entries {
			if entry.IsDir() {
				return nil, errors.Errorf("a dir %q in the logfiles dir %q", entry.Name(), logfilesDir)
			}

			files = append(files, filepath.Join(logfilesDir, entry.Name()))
		}

		sort.Strings(files)

		return &resolvedLogFiles{
			files: files,
		}, nil

	case LogfilesKindJournalctl:
		if logfilesDescr.JournalctlDataFile == "" {
			return nil, errors.Errorf("kind is journalctl, but JournalctlDataFile is empty")
		}

		return &resolvedLogFiles{
			journalctlDataFile: filepath.Join(
				testCaseDir, logfilesDescr.JournalctlDataFile,
			),
		}, nil

	default:
		return nil, errors.Errorf("invalid logfiles kind %q", logfilesDescr.Kind)
	}
}

type provisionedLogFiles struct {
	logfileLast, logfilePrev string

	// extraEnv contains extra env vars in the format "VARIABLE=VALUE"
	extraEnv []string
}

func provisionLogFiles(resolved *resolvedLogFiles, testOutputDir, repoRoot string) (*provisionedLogFiles, error) {
	var logfileLast, logfilePrev string

	// extraEnv contains extra env vars in the format "VARIABLE=VALUE"
	var extraEnv []string

	if len(resolved.files) > 0 {
		logfiles := resolved.files
		if len(logfiles) == 0 || len(logfiles) > 2 {
			return nil, errors.Errorf(
				"For now, there must be exactly 1 or 2 logfiles, but got %d: %v",
				len(logfiles), logfiles,
			)
		}

		logfileLast = filepath.Join(testOutputDir, "logfile")
		logfilePrev = filepath.Join(testOutputDir, "logfile.1")

		if err := copyFile(logfiles[0], logfileLast); err != nil {
			return nil, errors.Annotatef(err, "copying logfile last: from %s to %s", logfiles[0], logfileLast)
		}

		if err := setSyslogFileModTime(logfileLast); err != nil {
			return nil, errors.Trace(err)
		}

		if len(logfiles) > 1 {
			if err := copyFile(logfiles[1], logfilePrev); err != nil {
				return nil, errors.Annotatef(err, "copying logfile prev: from %s to %s", logfiles[1], logfilePrev)
			}

			if err := setSyslogFileModTime(logfilePrev); err != nil {
				return nil, errors.Trace(err)
			}
		}
	} else if resolved.journalctlDataFile != "" {
		srcJournalctlMockDir := filepath.Join(repoRoot, "cmd", "journalctl_mock")
		srcJournalctlMockShFname := filepath.Join(srcJournalctlMockDir, "journalctl_mock.sh")
		srcJournalctlMockGoFname := filepath.Join(srcJournalctlMockDir, "journalctl_mock.go")
		srcJournalctlMockGoModFname := filepath.Join(srcJournalctlMockDir, "go.mod")
		srcJournalctlMockGoSumFname := filepath.Join(srcJournalctlMockDir, "go.sum")

		tgtJournalctlMockDir := filepath.Join(testOutputDir, "journalctl_mock")
		tgtJournalctlMockShFname := filepath.Join(tgtJournalctlMockDir, "journalctl_mock.sh")
		tgtJournalctlMockGoFname := filepath.Join(tgtJournalctlMockDir, "journalctl_mock.go")
		tgtJournalctlMockGoModFname := filepath.Join(tgtJournalctlMockDir, "go.mod")
		tgtJournalctlMockGoSumFname := filepath.Join(tgtJournalctlMockDir, "go.sum")

		if err := os.MkdirAll(tgtJournalctlMockDir, 0755); err != nil {
			return nil, errors.Errorf("unable to create journalctl_mock output dir %s: %s", tgtJournalctlMockDir, err.Error())
		}

		if err := copyFile(srcJournalctlMockShFname, tgtJournalctlMockShFname); err != nil {
			return nil, errors.Annotatef(err, "copying from %s to %s", srcJournalctlMockShFname, tgtJournalctlMockShFname)
		}
		if err := os.Chmod(tgtJournalctlMockShFname, 0755); err != nil {
			return nil, errors.Annotatef(err, "changing permissions for journalctl mock %s", tgtJournalctlMockShFname)
		}

		if err := copyFile(srcJournalctlMockGoFname, tgtJournalctlMockGoFname); err != nil {
			return nil, errors.Annotatef(err, "copying from %s to %s", srcJournalctlMockGoFname, tgtJournalctlMockGoFname)
		}

		if err := copyFile(srcJournalctlMockGoModFname, tgtJournalctlMockGoModFname); err != nil {
			return nil, errors.Annotatef(err, "copying from %s to %s", srcJournalctlMockGoModFname, tgtJournalctlMockGoModFname)
		}

		if err := copyFile(srcJournalctlMockGoSumFname, tgtJournalctlMockGoSumFname); err != nil {
			return nil, errors.Annotatef(err, "copying from %s to %s", srcJournalctlMockGoSumFname, tgtJournalctlMockGoSumFname)
		}

		// Special case for the journalctl, no need to copy any files.
		logfileLast = "journalctl"
		logfilePrev = "journalctl"
		extraEnv = append(
			extraEnv,
			fmt.Sprintf("NERDLOG_JOURNALCTL_MOCK=%s", tgtJournalctlMockShFname),
			fmt.Sprintf("NERDLOG_JOURNALCTL_MOCK_DATA=%s", resolved.journalctlDataFile),
		)
	} else {
		return nil, errors.Errorf(
			"For now, there must be exactly 1 or 2 logfiles, or journalctl data file, but got nothing",
		)
	}

	return &provisionedLogFiles{
		logfileLast: logfileLast,
		logfilePrev: logfilePrev,
		extraEnv:    extraEnv,
	}, nil
}

func copyFile(srcPath, destPath string) error {
	// Open the source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return errors.Annotatef(err, "opening source file")
	}
	defer srcFile.Close()

	// Create the destination file
	destFile, err := os.Create(destPath)
	if err != nil {
		return errors.Annotatef(err, "creating destination file")
	}
	defer destFile.Close()

	// Copy the contents of the source file to the destination file
	_, err = io.Copy(destFile, srcFile)
	if err != nil {
		return errors.Annotatef(err, "copying data")
	}

	// Ensure the destination file is written to disk
	err = destFile.Sync()
	if err != nil {
		return errors.Annotatef(err, "syncing destination file")
	}

	return nil
}

// Function to extract the latest timestamp from a syslog file
func getLatestSyslogTimestamp(filePath string) (time.Time, error) {
	// Open the syslog file
	file, err := os.Open(filePath)
	if err != nil {
		return time.Time{}, err
	}
	defer file.Close()

	// Regular expression to match a typical syslog timestamp (e.g., "Apr  5 14:33:22")
	re := regexp.MustCompile(`^([A-Za-z]{3} \s?\d{1,2} \d{2}:\d{2}:\d{2})`)

	var latestTimestamp time.Time

	// Read the file backwards (find the last line)
	var lastLine string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lastLine = scanner.Text() // Keep updating lastLine until the last line
	}

	if err := scanner.Err(); err != nil {
		return time.Time{}, err
	}

	// Extract the timestamp from the last line
	matches := re.FindStringSubmatch(lastLine)
	if len(matches) > 0 {
		timestampStr := matches[1]
		latestTimestamp, err = time.ParseInLocation("Jan 2 15:04:05", timestampStr, time.UTC)
		if err != nil {
			return time.Time{}, err
		}

		latestTimestamp = setYear(latestTimestamp, time.Now().Year())
	}

	return latestTimestamp, nil
}

// setSyslogFileModTime takes the path to a syslog file, and sets its modification
// time to the timestamp of the last log message from that file.
func setSyslogFileModTime(fname string) error {
	lastLogTime, err := getLatestSyslogTimestamp(fname)
	if err != nil {
		return errors.Annotatef(err, "getting timestamp of the last log message in %s", fname)
	}

	if err := os.Chtimes(fname, lastLogTime, lastLogTime); err != nil {
		return errors.Annotatef(err, "setting mod time of %s", fname)
	}

	return nil
}

func setYear(t time.Time, year int) time.Time {
	// Return a new time.Time with the desired year
	return time.Date(year, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// getTestCaseDirs scans the dir recursively and returns relative paths
// to all the dirs which contain the file "test_case.yaml". For example:
//
// []string{"mytest1_foo", "some_group/mytest1", "some_group/mytest2"}
func getTestCaseDirs(testCasesDir string, testDescrFname string) ([]string, error) {
	var result []string

	// Walk the directory recursively
	err := filepath.Walk(testCasesDir, func(path string, info os.FileInfo, err error) error {
		// If there's an error walking, wrap and return it
		if err != nil {
			return errors.Annotate(err, "failed to walk path: "+path)
		}

		// If it's a directory and contains testDescrFname
		if info.IsDir() {
			// Check if testDescrFname exists in the current directory
			testCaseFile := filepath.Join(path, testDescrFname)
			if _, err := os.Stat(testCaseFile); err == nil {
				// If the file exists, add the relative path to the result
				relPath, err := filepath.Rel(testCasesDir, path)
				if err != nil {
					return errors.Annotate(err, "failed to get relative path for: "+path)
				}
				// Add the relative path to the result
				result = append(result, relPath)
			}
		}
		return nil
	})

	if err != nil {
		return nil, errors.Annotate(err, "failed to scan directories")
	}

	return result, nil
}

// getNumLines returns the number of lines in the given file.
func getNumLines(fname string) (int, error) {
	file, err := os.Open(fname)
	if err != nil {
		return 0, errors.Annotatef(err, "failed to open file %q", fname)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}

	if err := scanner.Err(); err != nil {
		return 0, errors.Annotate(err, "error while scanning file")
	}

	return lineCount, nil
}

// getLastNonMatchingLine returns the number of the last line which does not start
// with the given string. Line numbers are 1-based.
func getLastNonMatchingLine(fname string, prefix string) (int, error) {
	file, err := os.Open(fname)
	if err != nil {
		return 0, errors.Annotatef(err, "failed to open file: %s", fname)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	lastNonMatchingLine := 0

	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if !strings.HasPrefix(line, prefix) {
			lastNonMatchingLine = lineNumber
		}
	}

	if err := scanner.Err(); err != nil {
		return 0, errors.Annotate(err, "error scanning file")
	}

	return lastNonMatchingLine, nil
}

// copyUpToNumLines copies srcPath as destPath, but only the first maxNumLines.
// It returns the last line.
func copyUpToNumLines(srcPath, destPath string, maxNumLines int) (string, error) {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return "", errors.Annotatef(err, "failed to open source file: %s", srcPath)
	}
	defer srcFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return "", errors.Annotatef(err, "failed to create destination file: %s", destPath)
	}
	defer destFile.Close()

	scanner := bufio.NewScanner(srcFile)
	writer := bufio.NewWriter(destFile)
	defer writer.Flush()

	var lastLine string
	lineCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		if lineCount < maxNumLines {
			_, err := writer.WriteString(line + "\n")
			if err != nil {
				return "", errors.Annotate(err, "failed to write line to destination file")
			}
		}
		lastLine = line
		lineCount++
		if lineCount >= maxNumLines {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return "", errors.Annotate(err, "error while scanning source file")
	}

	return lastLine, nil
}

func BenchmarkNerdlogAgentSmallLogNoIndex(b *testing.B) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	logfilesDir := filepath.Join(parentDir, "core_testdata", "input_logfiles", "small_mar")
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench1_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", filepath.Join(logfilesDir, "syslog"),
			"--logfile-prev", filepath.Join(logfilesDir, "syslog.1"),
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-12-10:00",
		},
	)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		os.Remove(indexFname)
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func BenchmarkNerdlogAgentSmallLogCompleteIndex(b *testing.B) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	logfilesDir := filepath.Join(parentDir, "core_testdata", "input_logfiles", "small_mar")
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench1_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", filepath.Join(logfilesDir, "syslog"),
			"--logfile-prev", filepath.Join(logfilesDir, "syslog.1"),
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-12-10:00",
		},
	)

	// Build the index
	os.Remove(indexFname)
	if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
		b.Fatalf("initial runNerdlogAgentForBenchmark failed: %s", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func BenchmarkNerdlogAgentLargeLogSmallPortionNoIndex(b *testing.B) {
	if err := generateLogfilesLarge(); err != nil {
		b.Fatalf("failed to generate log files: %s", err.Error())
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench_large_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", "/tmp/nerdlog_agent_test_output/randomlog_large",
			"--logfile-prev", "/tmp/nerdlog_agent_test_output/randomlog_large.1",
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-11-00:00",
		},
	)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		os.Remove(indexFname)
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func BenchmarkNerdlogAgentLargeLogSmallPortionCompleteIndex(b *testing.B) {
	if err := generateLogfilesLarge(); err != nil {
		b.Fatalf("failed to generate log files: %s", err.Error())
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench_large_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", "/tmp/nerdlog_agent_test_output/randomlog_large",
			"--logfile-prev", "/tmp/nerdlog_agent_test_output/randomlog_large.1",
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-11-00:00",
		},
	)

	// Build the index
	os.Remove(indexFname)
	if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
		b.Fatalf("initial runNerdlogAgentForBenchmark failed: %s", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func BenchmarkNerdlogAgentLargeLogTinyPortionCompleteIndex(b *testing.B) {
	if err := generateLogfilesLarge(); err != nil {
		b.Fatalf("failed to generate log files: %s", err.Error())
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench_large_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", "/tmp/nerdlog_agent_test_output/randomlog_large",
			"--logfile-prev", "/tmp/nerdlog_agent_test_output/randomlog_large.1",
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-11-01:30",
		},
	)

	// Build the index
	os.Remove(indexFname)
	if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
		b.Fatalf("initial runNerdlogAgentForBenchmark failed: %s", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func BenchmarkNerdlogAgentHugeLogOneHourCompleteIndex(b *testing.B) {
	if err := generateLogfilesHuge(); err != nil {
		b.Fatalf("failed to generate log files: %s", err.Error())
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("unable to get caller info")
	}

	parentDir := filepath.Dir(filename)
	nerdlogAgentShFname := filepath.Join(parentDir, "nerdlog_agent.sh")

	indexFname := filepath.Join(agentTestOutputRoot, "bench_huge_index")

	cmdArgs := append(
		[]string{
			nerdlogAgentShFname,
			"query",
			"--logfile-last", "/tmp/nerdlog_agent_test_output/randomlog_huge",
			"--logfile-prev", "/tmp/nerdlog_agent_test_output/randomlog_huge.1",
			"--index-file", indexFname,
			"--max-num-lines", "100",
			"--from", "2025-03-11-12:30",
		},
	)

	// Build the index
	os.Remove(indexFname)
	if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
		b.Fatalf("initial runNerdlogAgentForBenchmark failed: %s", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := runNerdlogAgentForBenchmark(cmdArgs); err != nil {
			b.Fatalf("runNerdlogAgentForBenchmark failed: %s", err)
		}
	}
}

func generateLogfilesLarge() error {
	t, err := time.Parse(time.RFC3339, "2025-03-09T06:00:00Z")
	if err != nil {
		return errors.Trace(err)
	}

	t2, err := time.Parse(time.RFC3339, "2025-03-10T06:00:00Z")
	if err != nil {
		return errors.Trace(err)
	}

	err = sysloggen.GenerateSyslog(sysloggen.Params{
		StartTime:     t,
		SecondLogTime: t2,

		LogBasename: "/tmp/nerdlog_agent_test_output/randomlog_large",

		NumLogs:    4000000,
		MinDelayMS: 0,
		MaxDelayMS: 80,

		RandomSeed: 123,

		SkipIfPrevLogSizeIs: 143841612,
		SkipIfLastLogSizeIs: 122432250,
	})
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

func generateLogfilesHuge() error {
	t, err := time.Parse(time.RFC3339, "2025-03-09T06:00:00Z")
	if err != nil {
		return errors.Trace(err)
	}

	t2, err := time.Parse(time.RFC3339, "2025-03-09T09:00:00Z")
	if err != nil {
		return errors.Trace(err)
	}

	err = sysloggen.GenerateSyslog(sysloggen.Params{
		StartTime:     t,
		SecondLogTime: t2,

		LogBasename: "/tmp/nerdlog_agent_test_output/randomlog_huge",

		NumLogs:    40000000,
		MinDelayMS: 0,
		MaxDelayMS: 10,

		RandomSeed: 123,

		SkipIfPrevLogSizeIs: 143833610,
		SkipIfLastLogSizeIs: 2518973348,
	})
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}
