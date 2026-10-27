// Package localworker supervises an embedded chiperka-worker process so a single
// container can both host the control panel and execute tests.
package localworker

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"chiperka-hub/internal/store"
)

const WorkerID = "local"

type Options struct {
	API           string // URL the worker uses to reach this server, e.g. http://127.0.0.1:8080
	Binary        string // path to chiperka-worker
	Concurrency   int
	Capacity      int
	MaxContainers int
}

// Run keeps the embedded worker alive until ctx is cancelled.
func Run(ctx context.Context, st *store.Store, opts Options, log *slog.Logger) {
	if _, err := exec.LookPath(opts.Binary); err != nil {
		log.Error("local worker disabled: chiperka-worker binary not found", "binary", opts.Binary)
		return
	}
	if os.Getenv("DOCKER_HOST") == "" {
		if _, err := os.Stat("/var/run/docker.sock"); err != nil {
			log.Warn("local worker: /var/run/docker.sock not found — mount it (-v /var/run/docker.sock:/var/run/docker.sock) or set DOCKER_HOST")
		}
	}

	backoff := time.Second
	for ctx.Err() == nil {
		token, err := provision(ctx, st)
		if err != nil {
			log.Error("local worker: provisioning failed", "err", err)
			return
		}
		started := time.Now()
		err = runOnce(ctx, opts, token, log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Warn("local worker exited, restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// provision makes sure the "local" worker row exists and binds it to a fresh token.
// Tokens are stored hashed, so a new one is minted on every (re)start.
func provision(ctx context.Context, st *store.Store) (string, error) {
	tokenID, secret, err := st.CreateToken(ctx, "worker local (embedded)", store.TokenWorker)
	if err != nil {
		return "", err
	}
	if _, err := st.GetWorker(ctx, WorkerID); err == store.ErrNotFound {
		return secret, st.CreateWorker(ctx, WorkerID, "local (embedded)", tokenID, true)
	} else if err != nil {
		return "", err
	}
	return secret, st.SetWorkerToken(ctx, WorkerID, tokenID)
}

func runOnce(ctx context.Context, opts Options, token string, log *slog.Logger) error {
	args := []string{"worker", "--api", opts.API}
	if opts.Concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(opts.Concurrency))
	}
	if opts.Capacity > 0 {
		args = append(args, "--capacity", strconv.Itoa(opts.Capacity))
	}
	if opts.MaxContainers > 0 {
		args = append(args, "--max-containers", strconv.Itoa(opts.MaxContainers))
	}
	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Env = append(os.Environ(), "CHIPERKA_WORKER_TOKEN="+token)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Info("local worker started", "pid", cmd.Process.Pid, "api", opts.API)
	go pipe(stdout, log)
	go pipe(stderr, log)
	return cmd.Wait()
}

func pipe(r io.Reader, log *slog.Logger) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		log.Info("worker: " + sc.Text())
	}
}
