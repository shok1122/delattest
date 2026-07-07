package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// outputCap bounds how much stdout/stderr we buffer from a running module,
// mirroring the fixed-capacity pipes used by the previous Rust implementation.
const outputCap = 1 * 1024 * 1024

func main() {
	host := getEnv("HOST", "0.0.0.0")
	port := getEnv("PORT", "3000")
	addr := net.JoinHostPort(host, port)

	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(router)}

	go func() {
		log.Printf("listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Println("Ctrl+C received. stopping the server")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func router(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		okText(w, http.StatusOK, "OK: POST /execute-wasm (body = WASI Core Module)")
	case r.Method == http.MethodPost && r.URL.Path == "/execute-wasm":
		text, err := handleExecuteWasm(r)
		if err != nil {
			okText(w, http.StatusBadRequest, fmt.Sprintf("WASM error: %v", err))
			return
		}
		okText(w, http.StatusOK, text)
	default:
		okText(w, http.StatusNotFound, "not found")
	}
}

func okText(w http.ResponseWriter, code int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(s))
}

func handleExecuteWasm(r *http.Request) (string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()

	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		return "", err
	}

	compiled, err := runtime.CompileModule(ctx, body)
	if err != nil {
		return "", err
	}

	stdout := newCappedBuffer(outputCap)
	stderr := newCappedBuffer(outputCap)

	config := wazero.NewModuleConfig().
		WithStdout(stdout).
		WithStderr(stderr)

	mod, err := runtime.InstantiateModule(ctx, compiled, config)
	if mod != nil {
		defer mod.Close(ctx)
	}
	if err != nil {
		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 0 {
			// _start called proc_exit(0); not an error.
		} else {
			return "", err
		}
	}

	out := stdout.String()
	errOut := stderr.String()
	if errOut == "" {
		return out, nil
	}
	return fmt.Sprintf("-- stdout --\n%s\n\n-- stderr --\n%s", out, errOut), nil
}

// cappedBuffer stops accepting bytes once it reaches its capacity instead of
// growing without bound, so a runaway module can't exhaust host memory.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}
