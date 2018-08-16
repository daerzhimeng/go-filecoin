package testhelpers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gx/ipfs/QmVmDhyTTUcQXFD1rRQ64fGLMSAoaQvNH3hwuaCFAPq2hy/errors"
	cid "gx/ipfs/QmYVNvtQkeZ6AKSwDrjQTs432QtL6umrrK41EBq3cu7iSP/go-cid"

	"github.com/filecoin-project/go-filecoin/config"
	"github.com/filecoin-project/go-filecoin/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// DefaultDaemonCmdTimeout is the default timeout for executing commands.
	DefaultDaemonCmdTimeout = 1 * time.Minute
)

// Output manages running, inprocess, a filecoin command.
type Output struct {
	lk sync.Mutex
	// Input is the the raw input we got.
	Input string
	// Args is the cleaned up version of the input.
	Args []string
	// Code is the unix style exit code, set after the command exited.
	Code int
	// Error is the error returned from the command, after it exited.
	Error  error
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	stdout []byte
	Stderr io.ReadCloser
	stderr []byte

	test testing.TB
}

// ReadStderr returns a string representation of the stderr output.
func (o *Output) ReadStderr() string {
	o.lk.Lock()
	defer o.lk.Unlock()

	return string(o.stderr)
}

// ReadStdout returns a string representation of the stdout output.
func (o *Output) ReadStdout() string {
	o.lk.Lock()
	defer o.lk.Unlock()

	return string(o.stdout)
}

// ReadStdoutTrimNewlines returns a string representation of stdout,
// with trailing line breaks removed.
func (o *Output) ReadStdoutTrimNewlines() string {
	// TODO: handle non unix line breaks
	return strings.Trim(o.ReadStdout(), "\n")
}

// RunSuccessFirstLine executes the given command, asserts success and returns
// the first line of stdout.
func RunSuccessFirstLine(td *TestDaemon, args ...string) string {
	return RunSuccessLines(td, args...)[0]
}

// RunSuccessLines executes the given command, asserts success and returns
// an array of lines of the stdout.
func RunSuccessLines(td *TestDaemon, args ...string) []string {
	output := td.RunSuccess(args...)
	result := output.ReadStdoutTrimNewlines()
	return strings.Split(result, "\n")
}

// TestDaemon is used to manage a Filecoin daemon instance for testing purposes.
type TestDaemon struct {
	cmdAddr     string
	swarmAddr   string
	repoDir     string
	walletFile  string
	walletAddr  string
	genesisFile string
	mockMine    bool
	keyFiles    []string

	firstRun bool
	init     bool

	// The filecoin daemon process
	process *exec.Cmd

	lk     sync.Mutex
	Stdin  io.Writer
	Stdout io.Reader
	Stderr io.Reader

	test *testing.T

	cmdTimeout time.Duration
}

// RepoDir returns the repo directory of the test daemon.
func (td *TestDaemon) RepoDir() string {
	return td.repoDir
}

// CmdAddr returns the command address of the test daemon.
func (td *TestDaemon) CmdAddr() string {
	return td.cmdAddr
}

// SwarmAddr returns the swarm address of the test daemon.
func (td *TestDaemon) SwarmAddr() string {
	return td.swarmAddr
}

// Run executes the given command against the test daemon.
func (td *TestDaemon) Run(args ...string) *Output {
	td.test.Helper()
	return td.RunWithStdin(nil, args...)
}

// RunWithStdin executes the given command against the test daemon, allowing to control
// stdin of the process.
func (td *TestDaemon) RunWithStdin(stdin io.Reader, args ...string) *Output {
	td.test.Helper()
	bin, err := GetFilecoinBinary()
	require.NoError(td.test, err)

	ctx, cancel := context.WithTimeout(context.Background(), td.cmdTimeout)
	defer cancel()

	// handle Run("cmd subcmd")
	if len(args) == 1 {
		args = strings.Split(args[0], " ")
	}

	finalArgs := append(args, "--repodir="+td.repoDir, "--cmdapiaddr="+td.cmdAddr)

	td.test.Logf("run: %q\n", strings.Join(finalArgs, " "))
	cmd := exec.CommandContext(ctx, bin, finalArgs...)

	if stdin != nil {
		cmd.Stdin = stdin
	}

	stderr, err := cmd.StderrPipe()
	require.NoError(td.test, err)

	stdout, err := cmd.StdoutPipe()
	require.NoError(td.test, err)

	require.NoError(td.test, cmd.Start())

	stderrBytes, err := ioutil.ReadAll(stderr)
	require.NoError(td.test, err)

	stdoutBytes, err := ioutil.ReadAll(stdout)
	require.NoError(td.test, err)

	o := &Output{
		Args:   args,
		Stdout: stdout,
		stdout: stdoutBytes,
		Stderr: stderr,
		stderr: stderrBytes,
		test:   td.test,
	}

	err = cmd.Wait()

	switch err := err.(type) {
	case *exec.ExitError:
		if ctx.Err() == context.DeadlineExceeded {
			o.Error = errors.Wrapf(err, "context deadline exceeded for command: %q", strings.Join(finalArgs, " "))
		}

		// TODO: its non-trivial to get the 'exit code' cross platform...
		o.Code = 1
	default:
		o.Error = err
	case nil:
		// okay
	}

	return o
}

// RunSuccess is like Run, but asserts that the command exited successfully.
func (td *TestDaemon) RunSuccess(args ...string) *Output {
	td.test.Helper()
	return td.Run(args...).AssertSuccess()
}

// AssertSuccess asserts that the output represents a successful execution.
func (o *Output) AssertSuccess() *Output {
	o.test.Helper()
	require.NoError(o.test, o.Error)
	oErr := o.ReadStderr()

	require.Equal(o.test, 0, o.Code, oErr)
	require.NotContains(o.test, oErr, "CRITICAL")
	require.NotContains(o.test, oErr, "ERROR")
	require.NotContains(o.test, oErr, "WARNING")
	require.NotContains(o.test, oErr, "Error:")

	return o

}

// RunFail is like Run, but asserts that the command exited with an error
// matching the passed in error.
func (td *TestDaemon) RunFail(err string, args ...string) *Output {
	td.test.Helper()
	return td.Run(args...).AssertFail(err)
}

// AssertFail asserts that the output represents a failed execution, with the error
// matching the passed in error.
func (o *Output) AssertFail(err string) *Output {
	o.test.Helper()
	require.NoError(o.test, o.Error)
	require.Equal(o.test, 1, o.Code)
	require.Empty(o.test, o.ReadStdout())
	require.Contains(o.test, o.ReadStderr(), err)
	return o
}

// GetID returns the id of the daemon.
func (td *TestDaemon) GetID() string {
	out := td.RunSuccess("id")
	var parsed map[string]interface{}
	require.NoError(td.test, json.Unmarshal([]byte(out.ReadStdout()), &parsed))

	return parsed["ID"].(string)
}

// GetAddress returns the first address of the daemon.
func (td *TestDaemon) GetAddress() string {
	out := td.RunSuccess("id")
	var parsed map[string]interface{}
	require.NoError(td.test, json.Unmarshal([]byte(out.ReadStdout()), &parsed))

	adders := parsed["Addresses"].([]interface{})
	return adders[0].(string)
}

// ConnectSuccess connects the daemon to another daemon, asserting that
// the operation was successful.
func (td *TestDaemon) ConnectSuccess(remote *TestDaemon) *Output {
	// Connect the nodes
	out := td.RunSuccess("swarm", "connect", remote.GetAddress())
	peers1 := td.RunSuccess("swarm", "peers")
	peers2 := remote.RunSuccess("swarm", "peers")

	td.test.Log("[success] 1 -> 2")
	require.Contains(td.test, peers1.ReadStdout(), remote.GetID())

	td.test.Log("[success] 2 -> 1")
	require.Contains(td.test, peers2.ReadStdout(), td.GetID())

	return out
}

// ReadStdout returns a string representation of the stdout of the daemon.
func (td *TestDaemon) ReadStdout() string {
	td.lk.Lock()
	defer td.lk.Unlock()
	out, err := ioutil.ReadAll(td.Stdout)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// ReadStderr returns a string representation of the stderr of the daemon.
func (td *TestDaemon) ReadStderr() string {
	td.lk.Lock()
	defer td.lk.Unlock()
	out, err := ioutil.ReadAll(td.Stderr)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// Start starts up the daemon.
func (td *TestDaemon) Start() *TestDaemon {
	require.NoError(td.test, td.process.Start())
	require.NoError(td.test, td.WaitForAPI(), "Daemon failed to start")

	// on first startup import key pairs, if defined
	if td.firstRun {
		for _, file := range td.keyFiles {
			td.RunSuccess("wallet", "import", file)
		}
	}

	return td
}

// Shutdown stops the daemon.
func (td *TestDaemon) Shutdown() {
	if err := td.process.Process.Signal(syscall.SIGTERM); err != nil {
		td.test.Errorf("Daemon Stderr:\n%s", td.ReadStderr())
		td.test.Fatalf("Failed to kill daemon %s", err)
	}

	if td.repoDir == "" {
		panic("testdaemon had no repodir set")
	}

	_ = os.RemoveAll(td.repoDir)
}

// ShutdownSuccess stops the daemon, asserting that it exited successfully.
func (td *TestDaemon) ShutdownSuccess() {
	err := td.process.Process.Signal(syscall.SIGTERM)
	assert.NoError(td.test, err)
	tdErr := td.ReadStderr()
	assert.NoError(td.test, err, tdErr)
	assert.NotContains(td.test, tdErr, "CRITICAL")
	assert.NotContains(td.test, tdErr, "ERROR")
	assert.NotContains(td.test, tdErr, "WARNING")
}

// ShutdownEasy stops the daemon using `SIGINT`.
func (td *TestDaemon) ShutdownEasy() {
	err := td.process.Process.Signal(syscall.SIGINT)
	assert.NoError(td.test, err)
	tdOut := td.ReadStderr()
	assert.NoError(td.test, err, tdOut)
}

// WaitForAPI polls if the API on the daemon is available, and blocks until
// it is.
func (td *TestDaemon) WaitForAPI() error {
	for i := 0; i < 100; i++ {
		err := tryAPICheck(td)
		if err == nil {
			return nil
		}
		time.Sleep(time.Millisecond * 100)
	}
	return fmt.Errorf("filecoin node failed to come online in given time period (20 seconds)")
}

// CreateMinerAddr issues a new message to the network, mines the message
// and returns the address of the new miner
// equivalent to:
//     `go-filecoin miner create --from $TEST_ACCOUNT 100000 20`
func (td *TestDaemon) CreateMinerAddr(fromAddr string) types.Address {
	// need money
	td.RunSuccess("mining", "once")

	var wg sync.WaitGroup
	var minerAddr types.Address

	wg.Add(1)
	go func() {
		miner := td.RunSuccess("miner", "create", "--from", fromAddr, "1000000", "1000")
		addr, err := types.NewAddressFromString(strings.Trim(miner.ReadStdout(), "\n"))
		require.NoError(td.test, err)
		require.NotEqual(td.test, addr, types.Address{})
		minerAddr = addr
		wg.Done()
	}()

	// ensure mining runs after the command in our goroutine
	td.RunSuccess("mpool --wait-for-count=1")
	td.RunSuccess("mining", "once")

	wg.Wait()

	return minerAddr
}

// WaitForMessageRequireSuccess accepts a message cid and blocks until a message with matching cid is included in a
// block. The receipt is then inspected to ensure that the corresponding message receipt had a 0 exit code.
func (td *TestDaemon) WaitForMessageRequireSuccess(msgCid *cid.Cid) {
	args := []string{"message", "wait", msgCid.String(), "--receipt=true", "--message=false"}
	trim := strings.Trim(td.RunSuccess(args...).ReadStdout(), "\n")
	rcpt := &types.MessageReceipt{}
	require.NoError(td.test, json.Unmarshal([]byte(trim), rcpt))
	require.Equal(td.test, 0, int(rcpt.ExitCode))
}

// CreateWalletAddr adds a new address to the daemons wallet and
// returns it.
// equivalent to:
//     `go-filecoin wallet addrs new`
func (td *TestDaemon) CreateWalletAddr() string {
	td.test.Helper()
	outNew := td.RunSuccess("wallet", "addrs", "new")
	addr := strings.Trim(outNew.ReadStdout(), "\n")
	require.NotEmpty(td.test, addr)
	return addr
}

// Config is a helper to read out the config of the deamon
func (td *TestDaemon) Config() *config.Config {
	cfg, err := config.ReadFile(filepath.Join(td.repoDir, "config.toml"))
	require.NoError(td.test, err)
	return cfg
}

// MineAndPropagate mines a block and ensure the block has propagated to all `peers`
// by comparing the current head block of `td` with the head block of each peer in `peers`
func (td *TestDaemon) MineAndPropagate(wait time.Duration, peers ...*TestDaemon) {
	td.RunSuccess("mining", "once")
	// short circuit
	if peers == nil {
		return
	}
	// ensure all peers have same chain head as `td`
	td.MustHaveChainHeadBy(wait, peers)
}

// MustHaveChainHeadBy ensures all `peers` have the same chain head as `td`, by
// duration `wait`
func (td *TestDaemon) MustHaveChainHeadBy(wait time.Duration, peers []*TestDaemon) {
	// will signal all nodes have completed check
	done := make(chan struct{})
	var wg sync.WaitGroup

	expHeadBlks := td.GetChainHead()
	var expHead types.SortedCidSet
	for _, blk := range expHeadBlks {
		expHead.Add(blk.Cid())
	}

	for _, p := range peers {
		wg.Add(1)
		go func(p *TestDaemon) {
			for {
				actHeadBlks := p.GetChainHead()
				var actHead types.SortedCidSet
				for _, blk := range actHeadBlks {
					actHead.Add(blk.Cid())
				}
				if expHead.Equals(actHead) {
					wg.Done()
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}(p)
	}

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-done:
		return
	case <-time.After(wait):
		td.test.Fatal("Timeout waiting for chains to sync")
	}
}

// GetChainHead returns the blocks in the head tipset from `td`
func (td *TestDaemon) GetChainHead() []types.Block {
	out := td.RunSuccess("chain", "ls", "--enc=json")
	bc := td.MustUnmarshalChain(out.ReadStdout())
	return bc[0]
}

// MustUnmarshalChain unmarshals the chain from `input` into a slice of blocks
func (td *TestDaemon) MustUnmarshalChain(input string) [][]types.Block {
	chain := strings.Trim(input, "\n")
	var bs [][]types.Block

	for _, line := range bytes.Split([]byte(chain), []byte{'\n'}) {
		var b []types.Block
		if err := json.Unmarshal(line, &b); err != nil {
			td.test.Fatal(err)
		}
		bs = append(bs, b)
	}

	return bs
}

// MakeMoney mines a block and ensures that the block has been propagated to all peers.
func (td *TestDaemon) MakeMoney(rewards int, peers ...*TestDaemon) {
	for i := 0; i < rewards; i++ {
		td.MineAndPropagate(time.Second*1, peers...)
	}
}

// MakeDeal will make a deal with the miner `miner`, using data `dealData`.
// MakeDeal will return the cid of `dealData`
func (td *TestDaemon) MakeDeal(dealData string, miner *TestDaemon, fromAddr string) string {

	// The daemons need 2 monies each.
	td.MakeMoney(2)
	miner.MakeMoney(2)

	// How long to wait for miner blocks to propagate to other nodes
	propWait := time.Second * 3

	m := miner.CreateMinerAddr(fromAddr)

	askO := miner.RunSuccess(
		"miner", "add-ask",
		"--from", fromAddr,
		m.String(), "1200", "1",
	)
	miner.MineAndPropagate(propWait, td)
	miner.RunSuccess("message", "wait", "--return", strings.TrimSpace(askO.ReadStdout()))

	td.RunSuccess(
		"client", "add-bid",
		"--from", fromAddr,
		"500", "1",
	)
	td.MineAndPropagate(propWait, miner)

	buf := strings.NewReader(dealData)
	o := td.RunWithStdin(buf, "client", "import").AssertSuccess()
	ddCid := strings.TrimSpace(o.ReadStdout())

	negidO := td.RunSuccess("client", "propose-deal", "--ask=0", "--bid=0", ddCid)

	miner.MineAndPropagate(propWait, td)

	negid := strings.Split(strings.Split(negidO.ReadStdout(), "\n")[1], " ")[1]
	// ensure we have made the deal
	td.RunSuccess("client", "query-deal", negid)
	// return the cid for the dealData (ddCid)
	return ddCid
}

func tryAPICheck(td *TestDaemon) error {
	url := fmt.Sprintf("http://127.0.0.1%s/api/id", td.cmdAddr)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}

	out := make(map[string]interface{})
	err = json.NewDecoder(resp.Body).Decode(&out)
	if err != nil {
		return fmt.Errorf("liveness check failed: %s", err)
	}

	_, ok := out["ID"]
	if !ok {
		return fmt.Errorf("liveness check failed: ID field not present in output")
	}

	return nil
}

// SwarmAddr allows setting the `swarmAddr` config option on the daemon.
func SwarmAddr(addr string) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.swarmAddr = addr
	}
}

// RepoDir allows setting the `repoDir` config option on the daemon.
func RepoDir(dir string) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.repoDir = dir
	}
}

// ShouldInit allows setting the `init` config option on the daemon. If
// set, `go-filecoin init` is run before starting up the daemon.
func ShouldInit(i bool) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.init = i
	}
}

// CmdTimeout allows setting the `cmdTimeout` config option on the daemon.
func CmdTimeout(t time.Duration) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.cmdTimeout = t
	}
}

// WalletFile allows setting the `walletFile` config option on the daemon.
func WalletFile(f string) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.walletFile = f
	}
}

// WalletAddr allows setting the `walletAddr` config option on the daemon.
func WalletAddr(a string) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.walletAddr = a
	}
}

// GenesisFile allows setting the `genesisFile` config option on the daemon.
func GenesisFile(a string) func(*TestDaemon) {
	return func(td *TestDaemon) {
		td.genesisFile = a
	}
}

// NewDaemon creates a new `TestDaemon`, using the passed in configuration options.
func NewDaemon(t *testing.T, options ...func(*TestDaemon)) *TestDaemon {
	// Ensure we have the actual binary
	filecoinBin, err := GetFilecoinBinary()
	if err != nil {
		t.Fatal(err)
	}

	//Ask the kernel for a port to avoid conflicts
	cmdPort, err := GetFreePort()
	if err != nil {
		t.Fatal(err)
	}
	swarmPort, err := GetFreePort()
	if err != nil {
		t.Fatal(err)
	}

	dir, err := ioutil.TempDir("", "go-fil-test")
	if err != nil {
		t.Fatal(err)
	}

	td := &TestDaemon{
		cmdAddr:     fmt.Sprintf(":%d", cmdPort),
		swarmAddr:   fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", swarmPort),
		test:        t,
		repoDir:     dir,
		init:        true, // we want to init unless told otherwise
		firstRun:    true,
		walletFile:  "",
		mockMine:    true, // mine without setting up a valid storage market in the chain state by default.
		cmdTimeout:  DefaultDaemonCmdTimeout,
		genesisFile: GenesisFilePath(), // default file includes all test addresses,
		keyFiles:    KeyFilePaths(),    // five default key pairs
	}

	// configure TestDaemon options
	for _, option := range options {
		option(td)
	}

	// build command options
	repoDirFlag := fmt.Sprintf("--repodir=%s", td.repoDir)
	cmdAPIAddrFlag := fmt.Sprintf("--cmdapiaddr=%s", td.cmdAddr)
	swarmListenFlag := fmt.Sprintf("--swarmlisten=%s", td.swarmAddr)
	walletFileFlag := fmt.Sprintf("--walletfile=%s", td.walletFile)
	walletAddrFlag := fmt.Sprintf("--walletaddr=%s", td.walletAddr)
	testGenesisFlag := fmt.Sprintf("--testgenesis=%t", td.walletFile != "")
	genesisFileFlag := fmt.Sprintf("--genesisfile=%s", td.genesisFile)
	mockMineFlag := ""

	if td.mockMine {
		mockMineFlag = "--mock-mine"
	}

	if td.init {
		out, err := RunInit(repoDirFlag, cmdAPIAddrFlag, walletFileFlag, walletAddrFlag, testGenesisFlag, genesisFileFlag)
		if err != nil {
			t.Log(string(out))
			t.Fatal(err)
		}
	}

	// define filecoin daemon process
	td.process = exec.Command(filecoinBin, "daemon", repoDirFlag, cmdAPIAddrFlag, mockMineFlag, swarmListenFlag)

	// setup process pipes
	td.Stdout, err = td.process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	td.Stderr, err = td.process.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	td.Stdin, err = td.process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	return td
}

// RunInit is the equivialent of executing `go-filecoin init`.
func RunInit(opts ...string) ([]byte, error) {
	return RunCommand("init", opts...)
}

// RunCommand executes the given cmd against `go-filecoin`.
func RunCommand(cmd string, opts ...string) ([]byte, error) {
	filecoinBin, err := GetFilecoinBinary()
	if err != nil {
		return nil, err
	}

	process := exec.Command(filecoinBin, append([]string{"init"}, opts...)...)
	return process.CombinedOutput()
}
