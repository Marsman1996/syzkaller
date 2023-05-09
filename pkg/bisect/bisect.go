// Copyright 2018 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package bisect

import (
	"fmt"
	"os"
	"time"

	"github.com/google/syzkaller/pkg/build"
	"github.com/google/syzkaller/pkg/debugtracer"
	"github.com/google/syzkaller/pkg/hash"
	"github.com/google/syzkaller/pkg/instance"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/report"
	"github.com/google/syzkaller/pkg/vcs"
)

type Config struct {
	Trace           debugtracer.DebugTracer
	Fix             bool
	DefaultCompiler string
	CompilerType    string
	Linker          string
	BinDir          string
	Ccache          string
	Timeout         time.Duration
	Kernel          KernelConfig
	Syzkaller       SyzkallerConfig
	Repro           ReproConfig
	Manager         *mgrconfig.Config
	BuildSemaphore  *instance.Semaphore
	TestSemaphore   *instance.Semaphore
}

type KernelConfig struct {
	Repo        string
	Branch      string
	Commit      string
	CommitTitle string
	Cmdline     string
	Sysctl      string
	Config      []byte
	// Baseline configuration is used in commit bisection. If the crash doesn't reproduce
	// with baseline configuratopm config bisection is run. When triggering configuration
	// option is found provided baseline configuration is modified according the bisection
	// results. This new configuration is tested once more with current head. If crash
	// reproduces with the generated configuration original configuation is replaced with
	// this minimized one.
	BaselineConfig []byte
	Userspace      string
}

type SyzkallerConfig struct {
	Repo         string
	Commit       string
	Descriptions string
}

type ReproConfig struct {
	Opts []byte
	Syz  []byte
	C    []byte
}

type env struct {
	cfg          *Config
	repo         vcs.Repo
	bisecter     vcs.Bisecter
	minimizer    vcs.ConfigMinimizer
	commit       *vcs.Commit
	head         *vcs.Commit
	kernelConfig []byte
	inst         instance.Env
	numTests     int
	startTime    time.Time
	buildTime    time.Duration
	testTime     time.Duration
	flaky        bool
}

const MaxNumTests = 20 // number of tests we do per commit

// Result describes bisection result:
// 1. if bisection is conclusive, the single cause/fix commit in Commits
//   - for cause bisection report is the crash on the cause commit
//   - for fix bisection report is nil
//   - Commit is nil
//   - NoopChange is set if the commit did not cause any change in the kernel binary
//     (bisection result it most likely wrong)
//
// 2. Bisected to a release commit
//   - if bisection is inconclusive, range of potential cause/fix commits in Commits
//   - report is nil in such case
//
// 3. Commit is nil
//   - if the crash still happens on the oldest release/HEAD (for cause/fix bisection correspondingly)
//   - no commits in Commits
//   - the crash report on the oldest release/HEAD;
//   - Commit points to the oldest/latest commit where crash happens.
//
// 4. Config contains kernel config used for bisection.
type Result struct {
	Commits    []*vcs.Commit
	Report     *report.Report
	Commit     *vcs.Commit
	Config     []byte
	NoopChange bool
	IsRelease  bool
}

type InfraError struct {
	Title string
}

func (e InfraError) Error() string {
	return e.Title
}

// Run does the bisection and returns either the Result,
// or, if the crash is not reproduced on the start commit, an error.
func Run(cfg *Config) (*Result, error) {
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	cfg.Manager.Cover = false // it's not supported somewhere back in time
	repo, err := vcs.NewRepo(cfg.Manager.TargetOS, cfg.Manager.Type, cfg.Manager.KernelSrc)
	if err != nil {
		return nil, err
	}
	inst, err := instance.NewEnv(cfg.Manager, cfg.BuildSemaphore, cfg.TestSemaphore)
	if err != nil {
		return nil, err
	}
	if _, err = repo.CheckoutBranch(cfg.Kernel.Repo, cfg.Kernel.Branch); err != nil {
		return nil, &InfraError{Title: fmt.Sprintf("%v", err)}
	}
	return runImpl(cfg, repo, inst)
}

func runImpl(cfg *Config, repo vcs.Repo, inst instance.Env) (*Result, error) {
	bisecter, ok := repo.(vcs.Bisecter)
	if !ok {
		return nil, fmt.Errorf("bisection is not implemented for %v", cfg.Manager.TargetOS)
	}
	minimizer, ok := repo.(vcs.ConfigMinimizer)
	if !ok && len(cfg.Kernel.BaselineConfig) != 0 {
		return nil, fmt.Errorf("config minimization is not implemented for %v", cfg.Manager.TargetOS)
	}
	env := &env{
		cfg:       cfg,
		repo:      repo,
		bisecter:  bisecter,
		minimizer: minimizer,
		inst:      inst,
		startTime: time.Now(),
	}
	head, err := repo.HeadCommit()
	if err != nil {
		return nil, err
	}
	defer env.repo.SwitchCommit(head.Hash)
	env.head = head
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unnamed host"
	}
	env.log("%s starts bisection %s", hostname, env.startTime.String())
	if cfg.Fix {
		env.log("bisecting fixing commit since %v", cfg.Kernel.Commit)
	} else {
		env.log("bisecting cause commit starting from %v", cfg.Kernel.Commit)
	}
	start := time.Now()
	res, err := env.bisect()
	if env.flaky {
		env.log("Reproducer flagged being flaky")
	}
	env.log("revisions tested: %v, total time: %v (build: %v, test: %v)",
		env.numTests, time.Since(start), env.buildTime, env.testTime)
	if err != nil {
		env.log("error: %v", err)
		return nil, err
	}
	if len(res.Commits) == 0 {
		if cfg.Fix {
			env.log("crash still not fixed on HEAD or HEAD had kernel test errors")
		} else {
			env.log("oldest tested release already had the bug or it had kernel test errors")
		}
		env.log("commit msg: %v", res.Commit.Title)
		if res.Report != nil {
			env.log("crash: %v\n%s", res.Report.Title, res.Report.Report)
		}
		return res, nil
	}
	what := "bad"
	if cfg.Fix {
		what = "good"
	}
	if len(res.Commits) > 1 {
		env.log("bisection is inconclusive, the first %v commit could be any of:", what)
		for _, com := range res.Commits {
			env.log("%v", com.Hash)
		}
		return res, nil
	}
	com := res.Commits[0]
	env.log("first %v commit: %v %v", what, com.Hash, com.Title)
	env.log("recipients (to): %q", com.Recipients.GetEmails(vcs.To))
	env.log("recipients (cc): %q", com.Recipients.GetEmails(vcs.Cc))
	if res.Report != nil {
		env.log("crash: %v\n%s", res.Report.Title, res.Report.Report)
	}
	return res, nil
}

func (env *env) bisect() (*Result, error) {
	err := env.bisecter.PrepareBisect()
	if err != nil {
		return nil, err
	}

	cfg := env.cfg
	if err := build.Clean(cfg.Manager.TargetOS, cfg.Manager.TargetVMArch,
		cfg.Manager.Type, cfg.Manager.KernelSrc); err != nil {
		return nil, fmt.Errorf("kernel clean failed: %v", err)
	}
	env.log("building syzkaller on %v", cfg.Syzkaller.Commit)
	if _, err := env.inst.BuildSyzkaller(cfg.Syzkaller.Repo, cfg.Syzkaller.Commit); err != nil {
		return nil, err
	}

	cfg.Kernel.Commit, err = env.identifyRewrittenCommit()
	if err != nil {
		return nil, err
	}
	com, err := env.repo.SwitchCommit(cfg.Kernel.Commit)
	if err != nil {
		return nil, err
	}

	env.log("ensuring issue is reproducible on original commit %v\n", cfg.Kernel.Commit)
	env.commit = com
	env.kernelConfig = cfg.Kernel.Config
	testRes, err := env.test()
	if err != nil {
		return nil, err
	} else if testRes.verdict != vcs.BisectBad {
		return nil, fmt.Errorf("the crash wasn't reproduced on the original commit")
	}

	if len(cfg.Kernel.BaselineConfig) != 0 {
		testRes1, err := env.minimizeConfig()
		if err != nil {
			return nil, err
		}
		if testRes1 != nil {
			testRes = testRes1
		}
	}

	bad, good, results1, fatalResult, err := env.commitRange()
	if fatalResult != nil || err != nil {
		return fatalResult, err
	}

	results := map[string]*testResult{cfg.Kernel.Commit: testRes}
	for _, res := range results1 {
		results[res.com.Hash] = res
	}
	pred := func() (vcs.BisectResult, error) {
		testRes1, err := env.test()
		if err != nil {
			return 0, err
		}
		if cfg.Fix {
			if testRes1.verdict == vcs.BisectBad {
				testRes1.verdict = vcs.BisectGood
			} else if testRes1.verdict == vcs.BisectGood {
				testRes1.verdict = vcs.BisectBad
			}
		}
		results[testRes1.com.Hash] = testRes1
		return testRes1.verdict, err
	}
	commits, err := env.bisecter.Bisect(bad.Hash, good.Hash, cfg.Trace, pred)
	if err != nil {
		return nil, err
	}
	res := &Result{
		Commits: commits,
		Config:  env.kernelConfig,
	}
	if len(commits) == 1 {
		com := commits[0]
		testRes := results[com.Hash]
		if testRes == nil {
			return nil, fmt.Errorf("no result for culprit commit")
		}
		res.Report = testRes.rep
		isRelease, err := env.bisecter.IsRelease(com.Hash)
		if err != nil {
			env.log("failed to detect release: %v", err)
		}
		res.IsRelease = isRelease
		noopChange, err := env.detectNoopChange(results, com)
		if err != nil {
			env.log("failed to detect noop change: %v", err)
		}
		res.NoopChange = noopChange
	}
	return res, nil
}

func (env *env) identifyRewrittenCommit() (string, error) {
	cfg := env.cfg
	_, err := env.repo.CheckoutBranch(cfg.Kernel.Repo, cfg.Kernel.Branch)
	if err != nil {
		return cfg.Kernel.Commit, err
	}
	contained, err := env.repo.Contains(cfg.Kernel.Commit)
	if err != nil || contained {
		return cfg.Kernel.Commit, err
	}

	// We record the tested kernel commit when syzkaller triggers a crash. These commits can become
	// unreachable after the crash was found, when the history of the tested kernel branch was
	// rewritten. The commit might have been completely deleted from the branch or just changed in
	// some way. Some branches like linux-next are often and heavily rewritten (aka rebased).
	// This can also happen when changing the branch you fuzz in an existing syz-manager config.
	// This makes sense when a downstream kernel fork rebased on top of a new upstream version and
	// you don't want syzkaller to report all your old bugs again.
	if cfg.Kernel.CommitTitle == "" {
		// This can happen during a manual bisection, when only a hash is given.
		return cfg.Kernel.Commit, fmt.Errorf(
			"commit %v not reachable in branch '%v' and no commit title available",
			cfg.Kernel.Commit, cfg.Kernel.Branch)
	}
	commit, err := env.repo.GetCommitByTitle(cfg.Kernel.CommitTitle)
	if err != nil {
		return cfg.Kernel.Commit, err
	}
	if commit == nil {
		return cfg.Kernel.Commit, fmt.Errorf(
			"commit %v not reachable in branch '%v'", cfg.Kernel.Commit, cfg.Kernel.Branch)
	}
	env.log("rewritten commit %v reidentified by title '%v'\n", commit.Hash, cfg.Kernel.CommitTitle)
	return commit.Hash, nil
}

func (env *env) minimizeConfig() (*testResult, error) {
	// Find minimal configuration based on baseline to reproduce the crash.
	testResults := make(map[hash.Sig]*testResult)
	predMinimize := func(test []byte) (vcs.BisectResult, error) {
		env.kernelConfig = test
		testRes, err := env.test()
		if err != nil {
			return 0, err
		}
		testResults[hash.Hash(test)] = testRes
		return testRes.verdict, err
	}
	minConfig, err := env.minimizer.Minimize(env.cfg.Manager.SysTarget, env.cfg.Kernel.Config,
		env.cfg.Kernel.BaselineConfig, env.cfg.Trace, predMinimize)
	if err != nil {
		return nil, err
	}
	env.kernelConfig = minConfig
	return testResults[hash.Hash(minConfig)], nil
}

func (env *env) detectNoopChange(results map[string]*testResult, com *vcs.Commit) (bool, error) {
	testRes := results[com.Hash]
	if testRes.kernelSign == "" || len(com.Parents) != 1 {
		return false, nil
	}
	parent := com.Parents[0]
	parentRes := results[parent]
	if parentRes == nil {
		env.log("parent commit %v wasn't tested", parent)
		// We could not test the parent commit if it is not based on the previous release
		// (instead based on an older release, i.e. a very old non-rebased commit
		// merged into the current release).
		// TODO: we can use a differnet compiler for this old commit
		// since effectively it's in the older release, in that case we may not
		// detect noop change anyway.
		if _, err := env.repo.SwitchCommit(parent); err != nil {
			return false, err
		}
		_, kernelSign, err := env.build()
		if err != nil {
			return false, err
		}
		parentRes = &testResult{kernelSign: kernelSign}
	}
	env.log("culprit signature: %v", testRes.kernelSign)
	env.log("parent  signature: %v", parentRes.kernelSign)
	return testRes.kernelSign == parentRes.kernelSign, nil
}

func (env *env) commitRange() (*vcs.Commit, *vcs.Commit, []*testResult, *Result, error) {
	rangeFunc := env.commitRangeForCause
	if env.cfg.Fix {
		rangeFunc = env.commitRangeForFix
	}

	bad, good, results1, err := rangeFunc()
	if err != nil {
		return bad, good, results1, nil, err
	}

	fatalResult, err := env.validateCommitRange(bad, good, results1)
	return bad, good, results1, fatalResult, err
}

func (env *env) commitRangeForFix() (*vcs.Commit, *vcs.Commit, []*testResult, error) {
	env.log("testing current HEAD %v", env.head.Hash)
	if _, err := env.repo.SwitchCommit(env.head.Hash); err != nil {
		return nil, nil, nil, err
	}
	res, err := env.test()
	if err != nil {
		return nil, nil, nil, err
	}
	if res.verdict != vcs.BisectGood {
		return env.head, nil, []*testResult{res}, nil
	}
	return env.head, env.commit, []*testResult{res}, nil
}

func (env *env) commitRangeForCause() (*vcs.Commit, *vcs.Commit, []*testResult, error) {
	cfg := env.cfg
	tags, err := env.bisecter.PreviousReleaseTags(cfg.Kernel.Commit, cfg.CompilerType)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(tags) == 0 {
		return nil, nil, nil, fmt.Errorf("no release tags before this commit")
	}
	lastBad := env.commit
	var results []*testResult
	for _, tag := range tags {
		env.log("testing release %v", tag)
		com, err := env.repo.SwitchCommit(tag)
		if err != nil {
			return nil, nil, nil, err
		}
		res, err := env.test()
		if err != nil {
			return nil, nil, nil, err
		}
		results = append(results, res)
		if res.verdict == vcs.BisectGood {
			return lastBad, com, results, nil
		}
		if res.verdict == vcs.BisectBad {
			lastBad = com
		}
	}
	// All tags were vcs.BisectBad or vcs.BisectSkip.
	return lastBad, nil, results, nil
}

func (env *env) validateCommitRange(bad, good *vcs.Commit, results []*testResult) (*Result, error) {
	if len(results) < 1 {
		return nil, fmt.Errorf("commitRange returned no results")
	}

	finalResult := results[len(results)-1] // HEAD test for fix, oldest tested test for cause bisection
	if finalResult.verdict == vcs.BisectBad {
		// For cause bisection: Oldest tested release already had the bug. Giving up.
		// For fix bisection:   Crash still not fixed on HEAD. Leaving Result.Commits empty causes
		//                      syzbot to retry this bisection later.
		env.log("crash still not fixed/happens on the oldest tested release")
		return &Result{Report: finalResult.rep, Commit: bad, Config: env.kernelConfig}, nil
	}
	if finalResult.verdict == vcs.BisectSkip {
		if env.cfg.Fix {
			// HEAD is moving target. Sometimes changes break syzkaller fuzzing.
			// Leaving Result.Commits empty so syzbot retries this bisection again later.
			env.log("HEAD had kernel build, boot or test errors")
			return &Result{Report: finalResult.rep, Commit: bad, Config: env.kernelConfig}, nil
		}
		// The oldest tested release usually doesn't change. Retrying would give us the same result,
		// unless we change the syz-ci setup (e.g. new rootfs, new compilers).
		return nil, fmt.Errorf("oldest tested release had kernel build, boot or test errors")
	}

	return nil, nil
}

type testResult struct {
	verdict    vcs.BisectResult
	com        *vcs.Commit
	rep        *report.Report
	kernelSign string
}

func (env *env) build() (*vcs.Commit, string, error) {
	current, err := env.repo.HeadCommit()
	if err != nil {
		return nil, "", err
	}

	bisectEnv, err := env.bisecter.EnvForCommit(
		env.cfg.DefaultCompiler, env.cfg.CompilerType, env.cfg.BinDir, current.Hash, env.kernelConfig)
	if err != nil {
		return current, "", err
	}
	env.log("testing commit %v %v", current.Hash, env.cfg.CompilerType)
	buildStart := time.Now()
	mgr := env.cfg.Manager
	if err := build.Clean(mgr.TargetOS, mgr.TargetVMArch, mgr.Type, mgr.KernelSrc); err != nil {
		return current, "", fmt.Errorf("kernel clean failed: %v", err)
	}
	kern := &env.cfg.Kernel
	_, imageDetails, err := env.inst.BuildKernel(&instance.BuildKernelConfig{
		CompilerBin:  bisectEnv.Compiler,
		LinkerBin:    env.cfg.Linker,
		CcacheBin:    env.cfg.Ccache,
		UserspaceDir: kern.Userspace,
		CmdlineFile:  kern.Cmdline,
		SysctlFile:   kern.Sysctl,
		KernelConfig: bisectEnv.KernelConfig,
	})
	if imageDetails.CompilerID != "" {
		env.log("compiler: %v", imageDetails.CompilerID)
	}
	if imageDetails.Signature != "" {
		env.log("kernel signature: %v", imageDetails.Signature)
	}
	env.buildTime += time.Since(buildStart)
	return current, imageDetails.Signature, err
}

// Note: When this function returns an error, the bisection it was called from is aborted.
// Hence recoverable errors must be handled and the callers must treat testResult with care.
// e.g. testResult.verdict will be vcs.BisectSkip for a broken build, but err will be nil.
func (env *env) test() (*testResult, error) {
	cfg := env.cfg
	if cfg.Timeout != 0 && time.Since(env.startTime) > cfg.Timeout {
		return nil, fmt.Errorf("bisection is taking too long (>%v), aborting", cfg.Timeout)
	}
	current, kernelSign, err := env.build()
	res := &testResult{
		verdict:    vcs.BisectSkip,
		com:        current,
		kernelSign: kernelSign,
	}
	if current == nil {
		// This is not recoverable, as the caller must know which commit to skip.
		return res, fmt.Errorf("couldn't get repo HEAD: %v", err)
	}
	if err != nil {
		errInfo := fmt.Sprintf("failed building %v: ", current.Hash)
		if verr, ok := err.(*osutil.VerboseError); ok {
			errInfo += verr.Title
			env.saveDebugFile(current.Hash, 0, verr.Output)
		} else if verr, ok := err.(*build.KernelError); ok {
			errInfo += string(verr.Report)
			env.saveDebugFile(current.Hash, 0, verr.Output)
		} else {
			errInfo += err.Error()
			env.log("%v", err)
		}

		env.log("%s", errInfo)
		res.rep = &report.Report{Title: errInfo}
		return res, nil
	}

	numTests := MaxNumTests / 2
	if env.flaky || env.numTests == 0 {
		// Use twice as many instances if the bug is flaky and during initial testing
		// (as we don't know yet if it's flaky or not).
		numTests *= 2
	}
	env.numTests++

	testStart := time.Now()

	results, err := env.inst.Test(numTests, cfg.Repro.Syz, cfg.Repro.Opts, cfg.Repro.C)
	env.testTime += time.Since(testStart)
	if err != nil {
		problem := fmt.Sprintf("repro testing failure: %v", err)
		env.log(problem)
		return res, &InfraError{Title: problem}
	}
	bad, good, infra, rep := env.processResults(current, results)
	res.rep = rep
	res.verdict = vcs.BisectSkip
	if infra > len(results)/2 {
		// More than 1/2 of runs failed with infrastructure error,
		// no sense in continuing the bisection at the moment.
		return res, &InfraError{Title: "more than 50% of runs failed with an infra error"}
	}
	if bad != 0 {
		res.verdict = vcs.BisectBad
		if !env.flaky && bad < good {
			env.log("reproducer seems to be flaky")
			env.flaky = true
		}
	} else if good != 0 {
		res.verdict = vcs.BisectGood
	} else {
		res.rep = &report.Report{
			Title: fmt.Sprintf("failed testing reproducer on %v", current.Hash),
		}
	}
	// If all runs failed with a boot/test error, we just end up with BisectSkip.
	// TODO: when we start supporting boot/test error bisection, we need to make
	// processResults treat that verdit as "good".
	return res, nil
}

func (env *env) processResults(current *vcs.Commit, results []instance.EnvTestResult) (
	bad, good, infra int, rep *report.Report) {
	var verdicts []string
	for i, res := range results {
		if res.Error == nil {
			good++
			verdicts = append(verdicts, "OK")
			continue
		}
		switch err := res.Error.(type) {
		case *instance.TestError:
			if err.Infra {
				infra++
				verdicts = append(verdicts, fmt.Sprintf("infra problem: %v", err))
			} else if err.Boot {
				verdicts = append(verdicts, fmt.Sprintf("boot failed: %v", err))
			} else {
				verdicts = append(verdicts, fmt.Sprintf("basic kernel testing failed: %v", err))
			}
			output := err.Output
			if err.Report != nil {
				output = err.Report.Output
			}
			env.saveDebugFile(current.Hash, i, output)
		case *instance.CrashError:
			bad++
			rep = err.Report
			verdicts = append(verdicts, fmt.Sprintf("crashed: %v", err))
			output := err.Report.Report
			if len(output) == 0 {
				output = err.Report.Output
			}
			env.saveDebugFile(current.Hash, i, output)
		default:
			infra++
			verdicts = append(verdicts, fmt.Sprintf("failed: %v", err))
		}
	}
	unique := make(map[string]bool)
	for _, verdict := range verdicts {
		unique[verdict] = true
	}
	if len(unique) == 1 {
		env.log("all runs: %v", verdicts[0])
	} else {
		for i, verdict := range verdicts {
			env.log("run #%v: %v", i, verdict)
		}
	}
	return
}

func (env *env) saveDebugFile(hash string, idx int, data []byte) {
	env.cfg.Trace.SaveFile(fmt.Sprintf("%v.%v", hash, idx), data)
}

func checkConfig(cfg *Config) error {
	if !osutil.IsExist(cfg.BinDir) {
		return fmt.Errorf("bin dir %v does not exist", cfg.BinDir)
	}
	if cfg.Kernel.Userspace != "" && !osutil.IsExist(cfg.Kernel.Userspace) {
		return fmt.Errorf("userspace dir %v does not exist", cfg.Kernel.Userspace)
	}
	if cfg.Kernel.Sysctl != "" && !osutil.IsExist(cfg.Kernel.Sysctl) {
		return fmt.Errorf("sysctl file %v does not exist", cfg.Kernel.Sysctl)
	}
	if cfg.Kernel.Cmdline != "" && !osutil.IsExist(cfg.Kernel.Cmdline) {
		return fmt.Errorf("cmdline file %v does not exist", cfg.Kernel.Cmdline)
	}
	return nil
}

func (env *env) log(msg string, args ...interface{}) {
	env.cfg.Trace.Log(msg, args...)
}
