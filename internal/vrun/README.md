# vrun

Manages varnishd process lifecycle.

## Types

- `Manager` - Handles workspace preparation, secret generation, and process execution
- `Config` - Configuration for building varnishd command-line arguments

## Usage

```go
logger := slog.Default()
mgr := vrun.New("/var/run/varnish", logger, "/var/lib/varnish/instance")

if err := mgr.PrepareWorkspace(); err != nil {
    log.Fatal(err)
}

cfg := &vrun.Config{
    WorkDir:    "/var/run/varnish",
    AdminPort:  6082,
    VarnishDir: "/var/lib/varnish/instance",
    Listen:     []string{":8080,http", ":443,https"},
    Storage:    []string{"malloc,256m"},
    Params:     map[string]string{"thread_pool_min": "10"},
}
args := vrun.BuildArgs(cfg)

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

// Start returns a ready channel and is non-blocking
ready, err := mgr.Start(ctx, "", args)
if err != nil {
    log.Fatal(err)
}

// Wait for varnish to be ready
select {
case <-ready:
    log.Println("Varnish is ready to receive traffic")
case <-time.After(30 * time.Second):
    log.Fatal("Timeout waiting for varnish")
}

// Block until varnish exits (or cancel context to stop it)
if err := mgr.Wait(); err != nil {
    log.Printf("Varnish exited: %v", err)
}
```

## Notes

- `Start()` is non-blocking; returns a ready channel that closes when varnishd is ready
- `Wait()` blocks until varnishd exits; use context cancellation to stop the process
- VCL is not loaded at startup (`-f ""`); load via admin socket after start
- Secret file is written to `WorkDir/secret`
