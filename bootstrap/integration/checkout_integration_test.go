package integration

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buildkite/bintest"
)

func TestCheckingOutLocalGitProject(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	env := []string{
		"BUILDKITE_GIT_CLONE_FLAGS=-v",
		"BUILDKITE_GIT_CLEAN_FLAGS=-fdq",
	}

	// Actually execute git commands, but with expectations
	git := tester.
		MustMock(t, "git").
		PassthroughToLocalCommand()

	// But assert which ones are called
	git.ExpectAll([][]interface{}{
		{"clone", "-v", "--", tester.Repo.Path, "."},
		{"clean", "-fdq"},
		{"fetch", "-v", "--prune", "origin", "master"},
		{"checkout", "-f", "FETCH_HEAD"},
		{"clean", "-fdq"},
		{"--no-pager", "show", "HEAD", "-s", "--format=fuller", "--no-color"},
	})

	// Mock out the meta-data calls to the agent after checkout
	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(1)
	agent.
		Expect("meta-data", "set", "buildkite:git:commit", bintest.MatchAny()).
		AndExitWith(0)

	tester.RunAndCheck(t, env...)
}

func TestCheckingOutLocalGitProjectWithSubmodules(t *testing.T) {
	t.Parallel()

	// Git for windows seems to struggle with local submodules in the temp dir
	if runtime.GOOS == `windows` {
		t.Skip()
	}

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	submoduleRepo, err := createTestGitRespository()
	if err != nil {
		t.Fatal(err)
	}
	defer submoduleRepo.Close()

	out, err := tester.Repo.Execute("submodule", "add", submoduleRepo.Path)
	if err != nil {
		t.Fatalf("Adding submodule failed: %s", out)
	}

	out, err = tester.Repo.Execute("commit", "-am", "Add example submodule")
	if err != nil {
		t.Fatalf("Committing submodule failed: %s", out)
	}

	env := []string{
		"BUILDKITE_GIT_CLONE_FLAGS=-v",
		"BUILDKITE_GIT_CLEAN_FLAGS=-fdq",
	}

	// Actually execute git commands, but with expectations
	git := tester.
		MustMock(t, "git").
		PassthroughToLocalCommand()

	// But assert which ones are called
	git.ExpectAll([][]interface{}{
		{"clone", "-v", "--", tester.Repo.Path, "."},
		{"clean", "-fdq"},
		{"submodule", "foreach", "--recursive", "git", "clean", "-fdq"},
		{"fetch", "-v", "--prune", "origin", "master"},
		{"checkout", "-f", "FETCH_HEAD"},
		{"submodule", "sync", "--recursive"},
		{"submodule", "update", "--init", "--recursive", "--force"},
		{"submodule", "foreach", "--recursive", "git", "reset", "--hard"},
		{"clean", "-fdq"},
		{"submodule", "foreach", "--recursive", "git", "clean", "-fdq"},
		{"submodule", "foreach", "--recursive", "git", "ls-remote", "--get-url"},
		{"--no-pager", "show", "HEAD", "-s", "--format=fuller", "--no-color"},
	})

	// Mock out the meta-data calls to the agent after checkout
	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(1)
	agent.
		Expect("meta-data", "set", "buildkite:git:commit", bintest.MatchAny()).
		AndExitWith(0)

	tester.RunAndCheck(t, env...)
}

func TestCheckingOutSetsCorrectGitMetadataAndSendsItToBuildkite(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(1)

	agent.
		Expect("meta-data", "set", "buildkite:git:commit",
			bintest.MatchPattern(`^commit`)).
		AndExitWith(0)

	tester.RunAndCheck(t)
}

func TestCheckingOutWithSSHKeyscan(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	tester.MustMock(t, "ssh-keyscan").
		Expect("github.com").
		AndWriteToStdout("github.com ssh-rsa xxx=").
		AndExitWith(0)

	git := tester.MustMock(t, "git")
	git.IgnoreUnexpectedInvocations()

	git.Expect("clone", "-v", "--", "git@github.com:buildkite/agent.git", ".").
		AndExitWith(0)

	env := []string{
		`BUILDKITE_REPO=git@github.com:buildkite/agent.git`,
		`BUILDKITE_SSH_KEYSCAN=true`,
	}

	tester.RunAndCheck(t, env...)
}

func TestCheckingOutWithoutSSHKeyscan(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	tester.MustMock(t, "ssh-keyscan").
		Expect("github.com").
		NotCalled()

	env := []string{
		`BUILDKITE_REPO=https://github.com/buildkite/bash-example.git`,
		`BUILDKITE_SSH_KEYSCAN=false`,
	}

	tester.RunAndCheck(t, env...)
}

func TestCheckingOutWithSSHKeyscanAndUnscannableRepo(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	tester.MustMock(t, "ssh-keyscan").
		Expect("github.com").
		NotCalled()

	git := tester.MustMock(t, "git")
	git.IgnoreUnexpectedInvocations()

	git.Expect("clone", "-v", "--", "https://github.com/buildkite/bash-example.git", ".").
		AndExitWith(0)

	env := []string{
		`BUILDKITE_REPO=https://github.com/buildkite/bash-example.git`,
		`BUILDKITE_SSH_KEYSCAN=true`,
	}

	tester.RunAndCheck(t, env...)
}

func TestCleaningAnExistingCheckout(t *testing.T) {
	t.Parallel()

	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	// Create an existing checkout
	out, err := tester.Repo.Execute("clone", "-v", "--", tester.Repo.Path, tester.CheckoutDir())
	if err != nil {
		t.Fatalf("Clone failed with %s", out)
	}
	err = ioutil.WriteFile(filepath.Join(tester.CheckoutDir(), "test.txt"), []byte("llamas"), 0700)
	if err != nil {
		t.Fatalf("Write failed with %s", out)
	}

	// Mock out the meta-data calls to the agent after checkout
	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(0)

	tester.RunAndCheck(t)

	_, err = os.Stat(filepath.Join(tester.CheckoutDir(), "test.txt"))
	if os.IsExist(err) {
		t.Fatalf("test.txt still exitst")
	}
}

func TestForcingACleanCheckout(t *testing.T) {
	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	// Mock out the meta-data calls to the agent after checkout
	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(0)

	tester.RunAndCheck(t, "BUILDKITE_CLEAN_CHECKOUT=true")

	if !strings.Contains(tester.Output, "Cleaning pipeline checkout") {
		t.Fatalf("Should have removed checkout dir")
	}
}

func TestCheckoutOnAnExistingRepositoryWithoutAGitFolder(t *testing.T) {
	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	// Create an existing checkout
	out, err := tester.Repo.Execute("clone", "-v", "--", tester.Repo.Path, tester.CheckoutDir())
	if err != nil {
		t.Fatalf("Clone failed with %s", out)
	}

	if err = os.RemoveAll(filepath.Join(tester.CheckoutDir(), ".git", "refs")); err != nil {
		t.Fatal(err)
	}

	agent := tester.MustMock(t, "buildkite-agent")
	agent.
		Expect("meta-data", "exists", "buildkite:git:commit").
		AndExitWith(0)

	tester.RunAndCheck(t)
}

func TestCheckoutRetriesOnCleanFailure(t *testing.T) {
	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	var cleanCounter int32

	// Mock out all git commands, passing them through to the real thing unless it's a checkout
	git := tester.MustMock(t, "git").PassthroughToLocalCommand().Before(func(i bintest.Invocation) error {
		if i.Args[0] == "clean" {
			c := atomic.AddInt32(&cleanCounter, 1)

			// NB: clean gets run twice per checkout
			if c == 1 {
				return errors.New("Sunspots have caused git clean to fail")
			}
		}
		return nil
	})

	git.Expect().AtLeastOnce().WithAnyArguments()
	tester.RunAndCheck(t)
}

func TestCheckoutRetriesOnCloneFailure(t *testing.T) {
	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	var cloneCounter int32

	// Mock out all git commands, passing them through to the real thing unless it's a checkout
	git := tester.MustMock(t, "git").PassthroughToLocalCommand().Before(func(i bintest.Invocation) error {
		if i.Args[0] == "clone" {
			c := atomic.AddInt32(&cloneCounter, 1)
			if c == 1 {
				return errors.New("Sunspots have caused git clone to fail")
			}
		}
		return nil
	})

	git.Expect().AtLeastOnce().WithAnyArguments()
	tester.RunAndCheck(t)
}

func TestCheckoutDoesNotRetryOnHookFailure(t *testing.T) {
	tester, err := NewBootstrapTester()
	if err != nil {
		t.Fatal(err)
	}
	defer tester.Close()

	var checkoutCounter int32

	tester.ExpectGlobalHook("checkout").Once().AndCallFunc(func(c *bintest.Call) {
		counter := atomic.AddInt32(&checkoutCounter, 1)
		fmt.Fprintf(c.Stdout, "Checkout invocation %d\n", counter)
		if counter == 1 {
			fmt.Fprintf(c.Stdout, "Sunspots have caused checkout to fail\n")
			c.Exit(1)
		} else {
			c.Exit(0)
		}
	})

	if err = tester.Run(t); err == nil {
		t.Fatal("Expected the bootstrap to fail")
	}

	tester.CheckMocks(t)
}
