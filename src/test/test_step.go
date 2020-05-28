package test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/op/go-logging.v1"

	"github.com/thought-machine/please/src/build"
	"github.com/thought-machine/please/src/core"
	"github.com/thought-machine/please/src/fs"
	"github.com/thought-machine/please/src/utils"
	"github.com/thought-machine/please/src/worker"
)

var log = logging.MustGetLogger("test")

const dummyOutput = "=== RUN DummyTest\n--- PASS: DummyTest (0.00s)\nPASS\n"
const dummyCoverage = "<?xml version=\"1.0\" ?><coverage></coverage>"

// Tag that we attach for xattrs to store hashes against files.
// Note that we are required to provide the user namespace; that seems to be set implicitly
// by the attr utility, but that is not done for us here.
const xattrName = "user.plz_test"

// Test runs the tests for a single target.
func Test(tid int, state *core.BuildState, label core.BuildLabel, remote bool, run int) {
	state.LogBuildResult(tid, label, core.TargetTesting, "Testing...")
	target := state.Graph.TargetOrDie(label)
	test(tid, state.ForTarget(target), label, target, remote, run)
	if state.Config.Test.Upload != "" {
		if err := uploadResults(target, state.Config.Test.Upload.String()); err != nil {
			log.Warning("%s", err)
		}
	}
}

func test(tid int, state *core.BuildState, label core.BuildLabel, target *core.BuildTarget, runRemotely bool, run int) {
	hash, err := runtimeHash(state, target, runRemotely, run)
	if err != nil {
		state.LogBuildError(tid, label, core.TargetTestFailed, err, "Failed to calculate target hash")
		return
	}

	cachedOutputFile := target.TestResultsFile(run)
	cachedCoverageFile := target.CoverageFile(run)
	outputFile := path.Join(target.TestDir(run), core.TestResultsFile)
	coverageFile := path.Join(target.TestDir(run), core.CoverageFile)
	needCoverage := target.NeedCoverage(state)

	// If the user passed --shell then just prepare the directory.
	if state.PrepareShell {
		if err := prepareTestDir(state.Graph, target, run); err != nil {
			state.LogBuildError(tid, label, core.TargetTestFailed, err, "Failed to prepare test directory")
		} else {
			target.SetState(core.Stopped)
			state.LogBuildResult(tid, label, core.TargetTestStopped, "Test stopped")
		}
		return
	}

	cachedTestResults := func() *core.TestSuite {
		log.Debug("Not re-running test %s; got cached results.", label)
		coverage := parseCoverageFile(target, cachedCoverageFile, run)
		results, err := parseTestResults(cachedOutputFile, nil)
		results.Package = strings.Replace(target.Label.PackageName, "/", ".", -1)
		results.Name = target.Label.Name
		results.Cached = true
		if err != nil {
			state.LogBuildError(tid, label, core.TargetTestFailed, err, "Failed to parse cached test file %s", cachedOutputFile)
		} else if results.Failures() > 0 {
			log.Warning("Test results (for %s) with failures shouldn't be cached - ignoring.", label)
			state.Cache.Clean(target)
			return nil
		} else {
			logTestSuccess(state, tid, label, &results, coverage)
		}
		return &results
	}

	moveAndCacheOutputFiles := func(results *core.TestSuite, coverage *core.TestCoverage) bool {
		// Never cache test results when given arguments; the results may be incomplete.
		if len(state.TestArgs) > 0 {
			log.Debug("Not caching results for %s, we passed it arguments", label)
			return true
		}
		// Never cache test results if there were failures (usually flaky tests).
		if results.Failures() > 0 {
			log.Debug("Not caching results for %s, test had failures", label)
			return true
		}
		outs := []string{path.Base(cachedOutputFile)}
		if err := moveOutputFile(state, hash, outputFile, cachedOutputFile, dummyOutput); err != nil {
			state.LogTestResult(tid, label, core.TargetTestFailed, results, coverage, err, "Failed to move test output file")
			return false
		}
		if needCoverage || core.PathExists(coverageFile) {
			if err := moveOutputFile(state, hash, coverageFile, cachedCoverageFile, dummyCoverage); err != nil {
				state.LogTestResult(tid, label, core.TargetTestFailed, results, coverage, err, "Failed to move test coverage file")
				return false
			}
			outs = append(outs, path.Base(cachedCoverageFile))
		}
		for _, output := range target.TestOutputs {
			tmpFile := path.Join(target.TestDir(run), output)
			outFile := path.Join(target.OutDir(), output)
			if err := moveOutputFile(state, hash, tmpFile, outFile, ""); err != nil {
				state.LogTestResult(tid, label, core.TargetTestFailed, results, coverage, err, "Failed to move test output file")
				return false
			}
			outs = append(outs, output)
		}
		if state.Cache != nil {
			state.Cache.Store(target, hash, outs)
		}
		return true
	}

	needToRun := func() bool {
		if s := target.State(); (s == core.Unchanged || s == core.Reused) && core.PathExists(cachedOutputFile) {
			// Output file exists already and appears to be valid. We might still need to rerun though
			// if the coverage files aren't available.
			if needCoverage && !verifyHash(state, cachedCoverageFile, hash) {
				log.Debug("Rerunning %s, coverage file doesn't exist or has wrong hash", target.Label)
				return true
			} else if !verifyHash(state, cachedOutputFile, hash) {
				log.Debug("Rerunning %s, results file has incorrect hash", target.Label)
				return true
			}
			return false
		}
		log.Debug("Output file %s does not exist for %s", cachedOutputFile, target.Label)
		// Check the cache for these artifacts.
		files := []string{path.Base(target.TestResultsFile(run))}
		if needCoverage {
			files = append(files, path.Base(target.CoverageFile(run)))
		}
		return state.Cache == nil || !state.Cache.Retrieve(target, hash, files)
	}

	// Don't cache when doing multiple runs, presumably the user explicitly wants to check it.
	if state.NumTestRuns == 1 && !runRemotely && !needToRun() {
		if cachedResults := cachedTestResults(); cachedResults != nil {
			target.Results = *cachedResults
			return
		}
	}

	// If the test results haven't been initialised, initialise them now
	initialiseTargetResults(target)


	// Remove any cached test result file.
	if err := RemoveTestOutputs(target, run); err != nil {
		state.LogBuildError(tid, label, core.TargetTestFailed, err, "Failed to remove test output files")
		return
	}
	if worker, err := startTestWorkerIfNeeded(tid, state, target); err != nil {
		state.LogBuildError(tid, label, core.TargetTestFailed, err, "Failed to start test worker")
		testCase := core.TestCase{
			Name: worker,
			Executions: []core.TestExecution{
				{
					Failure: &core.TestResultFailure{
						Message:   "Failed to start test worker",
						Type:      "WorkerFail",
						Traceback: err.Error(),
					},
				},
			},
		}
		addTestCasesToTargetResult(target, core.TestCases{testCase})
		return
	}
	coverage := &core.TestCoverage{}


	//TODO(jpoole): check the parallelism flag here
	// Always run the test this number of times
	status := "Testing"
	var runStatus string
	var numFlakes int
	if state.NumTestRuns > 1 {
		runStatus = status + fmt.Sprintf(" (run %d of %d)", run, state.NumTestRuns)
		numFlakes = 1 // only run the test NumTestRuns times if this is greater than 1
	} else {
		runStatus = status
		// Run tests at least once, but possibly more if it's flaky.
		// Flakiness will be `3` if `flaky` is `True` in the build_def.
		numFlakes = utils.Max(target.Flakiness, 1)
	}
	// New group of test cases for each group of flaky runs
	flakeResults := core.TestSuite{}
	for flakes := 1; flakes <= numFlakes; flakes++ {
		var flakeStatus string
		if numFlakes > 1 {
			flakeStatus = runStatus + fmt.Sprintf(" (flake %d of %d)", flakes, numFlakes)
		} else {
			flakeStatus = runStatus
		}
		state.LogBuildResult(tid, label, core.TargetTesting, fmt.Sprintf("%s...", flakeStatus))

		testSuite, cov := doTest(tid, state, target, runRemotely, run)

		flakeResults.TimedOut = flakeResults.TimedOut || testSuite.TimedOut
		flakeResults.Properties = testSuite.Properties
		flakeResults.Duration += testSuite.Duration
		// Each set of executions is treated as a group
		// So if a test flakes three times, three executions will be part of one test case.
		flakeResults.Add(testSuite.TestCases...)
		coverage.Aggregate(cov)

		// If execution succeeded, we can break out of the flake loop
		if testSuite.TestCases.AllSucceeded() {
			break
		}

	}

	// Each set of executions is now treated separately
	// So if you ask for 3 runs you get 3 separate `PASS`es.
	target.ResultsMux.Lock()
	defer target.ResultsMux.Unlock()
	target.Results.Collapse(flakeResults)

	if state.NumTestRuns == 1 && target.Results.TestCases.AllSucceeded() && !runRemotely {
		// Success, store in cache
		moveAndCacheOutputFiles(&target.Results, coverage)
	}
	logTargetResults(tid, state, target, coverage, run)
}

func initialiseTargetResults(target *core.BuildTarget) {
	target.ResultsMux.Lock()
	defer target.ResultsMux.Unlock()

	if target.Results.Name == "" {
		target.Results = core.TestSuite{
			Package:   strings.Replace(target.Label.PackageName, "/", ".", -1),
			Name:      target.Label.Name,
			Timestamp: time.Now().Format(time.RFC3339),
		}
	}
}

func addTestCasesToTargetResult(target *core.BuildTarget, cases core.TestCases) {
	target.ResultsMux.Lock()
	defer target.ResultsMux.Unlock()
	target.Results.TestCases = append(target.Results.TestCases, cases...)
}

func logTargetResults(tid int, state *core.BuildState, target *core.BuildTarget, coverage *core.TestCoverage, run int) {
	if target.Results.TestCases.AllSucceeded() {
		// Clean up the test directory.
		if state.CleanWorkdirs {
			//TODO(jpoole): num runs
			if err := os.RemoveAll(target.TestDir(run)); err != nil {
				log.Warning("Failed to remove test directory for %s: %s", target.Label, err)
			}
		}
		logTestSuccess(state, tid, target.Label, &target.Results, coverage)
		return
	}
	var resultErr error
	var resultMsg string
	if target.Results.Failures() > 0 {
		resultMsg = "Tests failed"
		for _, testCase := range target.Results.TestCases {
			if len(testCase.Failures()) > 0 {
				resultErr = fmt.Errorf(testCase.Failures()[0].Failure.Message)
			}
		}
	} else if target.Results.Errors() > 0 {
		resultMsg = "Tests errored"
		for _, testCase := range target.Results.TestCases {
			if len(testCase.Errors()) > 0 {
				resultErr = fmt.Errorf(testCase.Errors()[0].Error.Message)
			}
		}
	} else {
		resultErr = fmt.Errorf("unknown error")
		resultMsg = "Something went wrong"
	}
	state.LogTestResult(tid, target.Label, core.TargetTestFailed, &target.Results, coverage, resultErr, resultMsg)
}

func logTestSuccess(state *core.BuildState, tid int, label core.BuildLabel, results *core.TestSuite, coverage *core.TestCoverage) {
	var description string
	tests := pluralise("test", results.Tests())
	if results.Skips() != 0 {
		description = fmt.Sprintf("%d %s passed. %d skipped",
			results.Tests(), tests, results.Skips())
	} else {
		description = fmt.Sprintf("%d %s passed.", len(results.TestCases), tests)
	}
	state.LogTestResult(tid, label, core.TargetTested, results, coverage, nil, description)
}

func pluralise(word string, quantity int) string {
	if quantity == 1 {
		return word
	}
	return word + "s"
}

func prepareTestDir(graph *core.BuildGraph, target *core.BuildTarget, run int) error {
	if err := os.RemoveAll(target.TestDir(run)); err != nil {
		return err
	}
	if err := os.MkdirAll(target.TestDir(run), core.DirPermissions); err != nil {
		return err
	}
	for out := range core.IterRuntimeFiles(graph, target, true, run) {
		if err := core.PrepareSourcePair(out); err != nil {
			return err
		}
	}
	return nil
}

// testCommandAndEnv returns the test command & environment for a target.
func testCommandAndEnv(state *core.BuildState, target *core.BuildTarget, run int) (string, []string, error) {
	replacedCmd, err := core.ReplaceTestSequences(state, target, target.GetTestCommand(state))
	env := core.TestEnvironment(state, target, path.Join(core.RepoRoot, target.TestDir(run)))
	if len(state.TestArgs) > 0 {
		args := strings.Join(state.TestArgs, " ")
		replacedCmd += " " + args
		env = append(env, "TESTS="+args)
	}
	return replacedCmd, env, err
}

func runTest(state *core.BuildState, target *core.BuildTarget, run int) ([]byte, error) {
	replacedCmd, env, err := testCommandAndEnv(state, target, run)
	if err != nil {
		return nil, err
	}
	log.Debugf("Running test %s#%d\nENVIRONMENT:\n%s\n%s", target.Label, run, strings.Join(env, "\n"), replacedCmd)
	_, stderr, err := state.ProcessExecutor.ExecWithTimeoutShellStdStreams(target, target.TestDir(run), env, target.TestTimeout, state.ShowAllOutput, replacedCmd, target.TestSandbox, state.DebugTests)
	return stderr, err
}

func doTest(tid int, state *core.BuildState, target *core.BuildTarget,  runRemotely bool, run int) (core.TestSuite, *core.TestCoverage) {
	startTime := time.Now()
	metadata, resultsData, coverage, err := doTestResults(tid, state, target, runRemotely, run)
	duration := time.Since(startTime)
	parsedSuite := parseTestOutput(metadata.Stdout, string(metadata.Stderr), err, duration, target, path.Join(target.TestDir(run), core.TestResultsFile), resultsData)

	return core.TestSuite{
		Package:    strings.Replace(target.Label.PackageName, "/", ".", -1),
		Name:       target.Label.Name,
		Duration:   duration,
		TimedOut:   err == context.DeadlineExceeded,
		Properties: parsedSuite.Properties,
		TestCases:  parsedSuite.TestCases,
	}, coverage
}

func doTestResults(tid int, state *core.BuildState, target *core.BuildTarget, runRemotely bool, run int) (*core.BuildMetadata, [][]byte, *core.TestCoverage, error) {
	if runRemotely {
		metadata, results, coverage, err := state.RemoteClient.Test(tid, target)
		cov, err2 := parseRemoteCoverage(state, target, coverage, run)
		if err == nil && err2 != nil {
			log.Error("Error parsing coverage data: %s", err2)
		}
		if metadata == nil {
			metadata = &core.BuildMetadata{}
		}
		return metadata, results, cov, err
	}
	stdout, err := prepareAndRunTest(tid, state, target, run)
	coverage := parseCoverageFile(target, path.Join(target.TestDir(run), core.CoverageFile), run)
	return &core.BuildMetadata{Stdout: stdout}, nil, coverage, err
}

func parseRemoteCoverage(state *core.BuildState, target *core.BuildTarget, coverage []byte, run int) (*core.TestCoverage, error) {
	if !state.NeedCoverage {
		return core.NewTestCoverage(), nil
	}
	return parseTestCoverage(target, coverage, run)
}

// prepareAndRunTest sets up a test directory and runs the test.
func prepareAndRunTest(tid int, state *core.BuildState, target *core.BuildTarget, run int) (stdout []byte, err error) {
	if err = prepareTestDir(state.Graph, target, run); err != nil {
		state.LogBuildError(tid, target.Label, core.TargetTestFailed, err, "Failed to prepare test directory for %s: %s", target.Label, err)
		return []byte{}, err
	}
	return runTest(state, target, run)
}

func parseTestOutput(stdout []byte, stderr string, runError error, duration time.Duration, target *core.BuildTarget, outputFile string, resultsData [][]byte) core.TestSuite {
	// This is all pretty involved; there are lots of different possibilities of what could happen.
	// The contract is that the test must return zero on success or non-zero on failure (Unix FTW).
	// If it's successful, it must produce a parseable file named "test.results" in its temp folder.
	// (alternatively, this can be a directory containing parseable files).
	// Tests can opt out of the file requirement individually, in which case they're judged only
	// by their return value.
	// But of course, we still have to consider all the alternatives here and handle them nicely.

	// No output and no execution error and output not expected - OK
	// No output and no execution error and output expected - SYNTHETIC ERROR - Missing Results
	// No output and execution error - SYNTHETIC ERROR - Failed to Run
	// Output and no execution error - PARSE OUTPUT - Ignore noTestOutput
	// Output and execution error - PARSE OUTPUT + SYNTHETIC ERROR - Incomplete Run
	if !fs.PathExists(outputFile) && len(resultsData) == 0 {
		if runError == nil && target.NoTestOutput {
			return core.TestSuite{
				TestCases: []core.TestCase{
					{
						// Need a name so that multiple runs get collated correctly.
						Name: target.Results.Name,
						Executions: []core.TestExecution{
							{
								Duration: &duration,
								Stdout:   string(stdout),
								Stderr:   stderr,
							},
						},
					},
				},
			}
		} else if runError == nil {
			return core.TestSuite{
				TestCases: []core.TestCase{
					{
						Name: target.Results.Name,
						Executions: []core.TestExecution{
							{
								Duration: &duration,
								Stdout:   string(stdout),
								Stderr:   stderr,
								Error: &core.TestResultFailure{
									Message: "Test failed to produce output results file",
									Type:    "MissingResults",
								},
							},
						},
					},
				},
			}
		} else if target.NoTestOutput {
			return core.TestSuite{
				TestCases: []core.TestCase{
					{
						Name: target.Results.Name,
						Executions: []core.TestExecution{
							{
								Duration: &duration,
								Stdout:   string(stdout),
								Stderr:   stderr,
								Failure: &core.TestResultFailure{
									Message: "Test failed: " + runError.Error(),
									Type:    "TestFailed",
								},
							},
						},
					},
				},
			}
		}

		return core.TestSuite{
			TestCases: []core.TestCase{
				{
					Name: target.Results.Name,
					Executions: []core.TestExecution{
						{
							Duration: &duration,
							Stdout:   string(stdout),
							Stderr:   stderr,
							Error: &core.TestResultFailure{
								Message:   "Test failed with no results",
								Type:      "NoResults",
								Traceback: runError.Error(),
							},
						},
					},
				},
			},
		}
	}

	results, parseError := parseTestResults(outputFile, resultsData)
	if parseError != nil {
		if runError != nil {
			return core.TestSuite{
				TestCases: []core.TestCase{
					{
						Name: target.Results.Name,
						Executions: []core.TestExecution{
							{
								Duration: &duration,
								Stdout:   string(stdout),
								Stderr:   stderr,
								Error: &core.TestResultFailure{
									Message:   "Test failed with no results",
									Type:      "NoResults",
									Traceback: runError.Error(),
								},
							},
						},
					},
				},
			}
		}

		return core.TestSuite{
			TestCases: []core.TestCase{
				{
					Name: "Unknown",
					Executions: []core.TestExecution{
						{
							Duration: &duration,
							Stdout:   string(stdout),
							Stderr:   stderr,
							Error: &core.TestResultFailure{
								Message:   "Couldn't parse test output file",
								Type:      "NoResults",
								Traceback: parseError.Error(),
							},
						},
					},
				},
			},
		}
	}

	if runError != nil && results.Failures() == 0 {
		// Add a failure result to the test so it shows up in the final aggregation.
		results.TestCases = append(results.TestCases, core.TestCase{
			// We don't know the type of test we ran :(
			Name: target.Results.Name,
			Executions: []core.TestExecution{
				{
					Duration: &duration,
					Stdout:   string(stdout),
					Stderr:   stderr,
					Error: &core.TestResultFailure{
						Type:      "ReturnValue",
						Message:   "Test returned nonzero but reported no errors",
						Traceback: runError.Error(),
					},
				},
			},
		})
	} else if runError == nil && results.Failures() != 0 {
		results.TestCases = append(results.TestCases, core.TestCase{
			// We don't know the type of test we ran :(
			Name: target.Results.Name,
			Executions: []core.TestExecution{
				{
					Duration: &duration,
					Stdout:   string(stdout),
					Stderr:   stderr,
					Failure: &core.TestResultFailure{
						Type:    "ReturnValue",
						Message: "Test returned 0 but still reported failures",
					},
				},
			},
		})
	}

	return results
}

// Parses the coverage output for a single target.
func parseCoverageFile(target *core.BuildTarget, coverageFile string, run int) *core.TestCoverage {
	coverage, err := parseTestCoverageFile(target, coverageFile, run)
	if err != nil {
		log.Errorf("Failed to parse coverage file for %s: %s", target.Label, err)
	}
	return coverage
}

// RemoveTestOutputs removes any cached test or coverage result files for a target.
func RemoveTestOutputs(target *core.BuildTarget, run int) error {
	if err := os.RemoveAll(target.TestResultsFile(run)); err != nil {
		return err
	} else if err := os.RemoveAll(target.CoverageFile(run)); err != nil {
		return err
	}
	for _, output := range target.TestOutputs {
		if err := os.RemoveAll(path.Join(target.OutDir(), output)); err != nil {
			return err
		}
	}
	return nil
}

// moveOutputFile moves an output file from the temporary directory to its permanent location.
// If dummy is given, it writes that into the destination if the file doesn't exist.
func moveOutputFile(state *core.BuildState, hash []byte, from, to, dummy string) error {
	if !core.PathExists(from) {
		if dummy == "" {
			return nil
		}
		if err := ioutil.WriteFile(to, []byte(dummy), 0644); err != nil {
			return err
		}
	} else if err := os.Rename(from, to); err != nil {
		return err
	}
	// Set the hash on the new destination file
	return fs.RecordAttr(to, hash, xattrName, state.XattrsSupported)
}

// startTestWorkerIfNeeded starts a worker server if the test needs one.
func startTestWorkerIfNeeded(tid int, state *core.BuildState, target *core.BuildTarget) (string, error) {
	workerCmd, _, testCmd, err := core.TestWorkerCommand(state, target)
	if err != nil {
		return "", err
	} else if workerCmd == "" {
		return "", nil
	}
	state.LogBuildResult(tid, target.Label, core.TargetTesting, "Starting test worker...")
	resp, err := worker.EnsureWorkerStarted(state, workerCmd, testCmd, target)
	if err == nil {
		state.LogBuildResult(tid, target.Label, core.TargetTesting, "Testing...")
		if resp.Command != "" {
			log.Debug("Setting test command for %s to %s", target.Label, resp.Command)
			target.TestCommand = resp.Command
		}
	}
	return workerCmd, err
}

// verifyHash verifies that the hash on a test file matches the one for the current test.
func verifyHash(state *core.BuildState, filename string, hash []byte) bool {
	return bytes.Equal(hash, fs.ReadAttr(filename, xattrName, state.XattrsSupported))
}

// runtimeHash returns the runtime hash of a target, or an empty slice if running remotely.
func runtimeHash(state *core.BuildState, target *core.BuildTarget, runRemotely bool, run int) ([]byte, error) {
	if runRemotely {
		return nil, nil
	}
	hash, err := build.RuntimeHash(state, target, run)
	if err == nil {
		hash = core.CollapseHash(hash)
	}
	return hash, err
}
