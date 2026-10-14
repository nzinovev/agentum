package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Runtime API paths the adapter reads. opencode 2 exposes its model and skill
// listings only through its HTTP API.
const (
	modelListPath = "/api/model"
	skillListPath = "/api/skill"
)

// runtimeServerUser is the basic-auth user opencode's server accepts; the
// password is what makes the server private.
const runtimeServerUser = "opencode"

// settleInterval and settleReads decide when a listing has settled. opencode
// boots a location's plugins on the first request for it and answers before
// they finish, and they finish in stages: measured on 2.0.29, no skills at
// 300ms, the two built-in ones at 430ms, all of them at 680ms. A listing is
// taken once it is non-empty and has not changed for settleReads consecutive
// reads — about a second, four times the gap between the stages. There is no
// "plugins settled" signal to wait for instead; plugin state reports only
// active or failed. The cost is paid once per run (skills) and once per
// process (models). Vars so the subprocess tests can shrink them.
var (
	settleInterval = 250 * time.Millisecond
	settleReads    = 5
)

// errListingEmpty reports a listing that never had a record before the probe
// deadline. It is a failed probe, not an empty result: opencode always
// registers its own built-in skill and its own models, so an empty answer
// means the loading never finished.
var errListingEmpty = errors.New("listing stayed empty")

// errBinaryNotFound is the probe reason for a runtime that is not installed,
// in the words the other probes record.
var errBinaryNotFound = errors.New("binary not found")

// runtimeServer is a private opencode server started for one probe. A probe
// cannot use `opencode api` against the shared background service: that
// service runs with its own environment, configuration, and plugin state, so
// it would describe itself rather than the environment an invocation gets.
// And a one-shot `opencode api --standalone` answers before the plugins have
// settled. A server of our own, started with the invocation's environment and
// polled until the listing settles, answers both.
type runtimeServer struct {
	cmd         *exec.Cmd
	baseURL     string
	password    string
	stderrTail  *tailBuffer
	reaped      chan struct{}
	watcherDone <-chan struct{}
	client      *http.Client
}

// startRuntimeServer starts `<binary> serve` on a loopback port the OS picks,
// in workdir and under env, and waits for it to report its address. ctx bounds
// the whole server: its cancellation kills the process group.
func (adapter *OpencodeAdapter) startRuntimeServer(ctx context.Context, workdir string, env []string) (*runtimeServer, error) {
	bin, err := exec.LookPath(adapter.binary)
	if err != nil {
		return nil, errBinaryNotFound
	}
	password, err := randomPassword()
	if err != nil {
		return nil, fmt.Errorf("server password: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "serve", "--hostname", "127.0.0.1", "--port", "0")
	setProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.Env = append(withWorkingDirectory(env, workdir), "OPENCODE_SERVER_PASSWORD="+password)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrTail := newTailBuffer(stderrTailLimit)
	cmd.Stderr = stderrTail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	server := &runtimeServer{
		cmd: cmd, password: password, stderrTail: stderrTail,
		reaped: make(chan struct{}),
		client: &http.Client{Timeout: probeTimeout},
	}
	// The address arrives on stdout; whatever follows is drained so the
	// server never blocks on a full pipe. The read ends at EOF, which comes
	// with the process exit, and the reap waits for it.
	addresses := make(chan string, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stdout)
		reported := false
		for scanner.Scan() {
			if reported {
				continue
			}
			if address := listeningAddress(scanner.Text()); address != "" {
				reported = true
				addresses <- address
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		<-readDone
		_ = cmd.Wait()
		close(server.reaped)
	}()
	server.watcherDone = watchCancellation(ctx, cmd, server.reaped)

	select {
	case address := <-addresses:
		server.baseURL = address
		return server, nil
	case <-server.reaped:
		return nil, withStderr(fmt.Errorf("exited: %v", cmd.ProcessState), stderrTail)
	case <-ctx.Done():
		<-server.reaped
		<-server.watcherDone
		return nil, ctx.Err()
	}
}

// listeningAddress extracts the URL from opencode's "server listening on
// <url>" line, or returns "" for any other line.
func listeningAddress(line string) string {
	const marker = "listening on "
	index := strings.Index(line, marker)
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(line[index+len(marker):])
}

// close stops the server and waits until it is reaped, so no probe leaves a
// server behind.
func (server *runtimeServer) close() {
	select {
	case <-server.reaped:
	default:
		terminateProcessGroup(server.cmd, server.reaped)
	}
	<-server.reaped
	<-server.watcherDone
}

// settledGet polls path until the listing has settled (see settleInterval)
// and returns its body. count reports how many records a body holds, or an
// error when the body does not decode — a decode failure is final, because a
// runtime that answers in an unknown shape will not change its mind.
func (server *runtimeServer) settledGet(ctx context.Context, path, workdir string, count func([]byte) (int, error)) ([]byte, error) {
	var previous []byte
	stableReads := 0
	for {
		body, err := server.get(ctx, path, workdir)
		if err != nil {
			// The deadline can land inside a request; a listing that never
			// had a record by then is still the empty-listing failure.
			if ctx.Err() != nil && len(previous) > 0 {
				if records, countErr := count(previous); countErr == nil && records == 0 {
					return nil, fmt.Errorf("%w: %w", errListingEmpty, ctx.Err())
				}
			}
			return nil, err
		}
		records, err := count(body)
		if err != nil {
			return nil, err
		}
		if records > 0 && bytes.Equal(body, previous) {
			stableReads++
		} else {
			stableReads = 1
		}
		previous = body
		if records > 0 && stableReads >= settleReads {
			return body, nil
		}
		select {
		case <-ctx.Done():
			if records == 0 {
				return nil, fmt.Errorf("%w: %w", errListingEmpty, ctx.Err())
			}
			return nil, ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// get performs one authenticated GET for workdir's location.
func (server *runtimeServer) get(ctx context.Context, path, workdir string) ([]byte, error) {
	query := url.Values{}
	query.Set("location[directory]", workdir)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(runtimeServerUser, server.password)
	response, err := server.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxListingBytes))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		// The status alone: an error body may echo configuration.
		return nil, fmt.Errorf("GET %s: HTTP %d", path, response.StatusCode)
	}
	return body, nil
}

// maxListingBytes bounds a listing read. Skill bodies travel inside the
// skill listing, so it is the larger of the two by far.
const maxListingBytes = 64 << 20

// randomPassword returns a fresh secret for one private server.
func randomPassword() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
