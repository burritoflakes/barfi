package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

type cliOptions struct {
	server       string
	token        string
	locationId   string
	parentId     string
	guestLink    string
	note         string
	partSizeStr  string
	workers      int
	configAction string // "show", "set", or "unset"
	save         bool
	quiet        bool
	jsonOutput   bool
	showHelp     bool
	showVersion  bool

	file       string
	configArgs []string // remaining positional args when --config is active
}

func parseFlags(args []string) (*cliOptions, error) {
	fs := flag.NewFlagSet("barfi", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	opts := &cliOptions{}
	fs.StringVar(&opts.server, "server", "", "BUS server base URL")
	fs.StringVar(&opts.token, "token", "", "Bearer token")
	fs.StringVar(&opts.locationId, "location-id", "", "Storage bucket id")
	fs.StringVar(&opts.locationId, "l", "", "Storage bucket id (short)")
	fs.StringVar(&opts.parentId, "parent-id", "", "Target directory id")
	fs.StringVar(&opts.parentId, "d", "", "Target directory id (short)")
	fs.StringVar(&opts.guestLink, "guest-upload-link-id", "", "Guest upload link id")
	fs.StringVar(&opts.note, "note", "", "Optional note, ≤500 chars")
	fs.StringVar(&opts.partSizeStr, "part-size", "", "Override auto part size (e.g. 25MB)")
	fs.IntVar(&opts.workers, "workers", 0, "Parallel upload workers (default 5)")
	fs.IntVar(&opts.workers, "j", 0, "Parallel upload workers (short)")
	fs.StringVar(&opts.configAction, "config", "", "Config management: show, set <key> <value>, unset <key>")
	fs.BoolVar(&opts.save, "save", false, "Persist resolved settings to config file")
	fs.BoolVar(&opts.quiet, "quiet", false, "Suppress progress output")
	fs.BoolVar(&opts.quiet, "q", false, "Suppress progress output (short)")
	fs.BoolVar(&opts.jsonOutput, "json", false, "Print server response as JSON")
	fs.BoolVar(&opts.showVersion, "version", false, "Print version and exit")
	fs.BoolVar(&opts.showHelp, "help", false, "Print help and exit")
	fs.BoolVar(&opts.showHelp, "h", false, "Print help and exit (short)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if opts.configAction != "" {
		// --config mode: positional args are key/value for set/unset, not a file path.
		opts.configArgs = rest
	} else {
		if len(rest) > 1 {
			return nil, fmt.Errorf("usage: barfi [flags] <file>")
		}
		if len(rest) == 1 {
			opts.file = rest[0]
		}
	}
	return opts, nil
}

// resolveSettings merges config file, env vars, and CLI flags with the
// precedence: config < env < flags.
func resolveSettings(opts *cliOptions) (Config, error) {
	var cfg Config
	path, err := defaultConfigPath()
	if err == nil {
		loaded, lerr := loadConfig(path)
		if lerr != nil {
			return cfg, lerr
		}
		cfg = loaded
	}
	// Env overrides.
	if v := os.Getenv("BARFI_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("BARFI_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("BARFI_LOCATION_ID"); v != "" {
		cfg.LocationId = v
	}
	// Flag overrides.
	if opts.server != "" {
		cfg.Server = opts.server
	}
	if opts.token != "" {
		cfg.Token = opts.token
	}
	if opts.locationId != "" {
		cfg.LocationId = opts.locationId
	}
	if opts.parentId != "" {
		cfg.ParentId = opts.parentId
	}
	if opts.workers > 0 {
		cfg.Workers = opts.workers
	}
	if cfg.Workers == 0 {
		cfg.Workers = 5
	}
	return cfg, nil
}

func printHelp() {
	_, _ = fmt.Fprintln(os.Stderr, `Usage: barfi [flags] <file>

Flags:
  --server URL               BUS server base URL (env: BARFI_SERVER)
  --token T                  Bearer token (env: BARFI_TOKEN)
  -l, --location-id ID       Storage bucket id (env: BARFI_LOCATION_ID)
  -d, --parent-id ID         Target directory id (requires --token)
      --guest-upload-link-id Guest upload link id
      --note TEXT            Optional note (≤500 chars, base64-encoded)
      --part-size BYTES      Override auto part size (e.g. 25MB)
  -j, --workers N             Parallel upload workers (default 5)
      --save                 Persist resolved settings to config file
      --config ACTION        Config management (show, set <key> <value>, unset <key>)
  -q, --quiet                Suppress progress output
      --json                 Print server response as JSON on completion
  -h, --help                 Print help and exit
      --version              Print version and exit

Config keys: server, token, locationId, parentId, workers`)
}

func runCLI(args []string) int {
	opts, err := parseFlags(args)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 2
	}
	if opts.showHelp {
		printHelp()
		return 0
	}
	if opts.showVersion {
		fmt.Println("barfi", version)
		return 0
	}

	// --config dispatches to config management and exits.
	if opts.configAction != "" {
		return runConfig(opts.configAction, opts.configArgs)
	}

	cfg, err := resolveSettings(opts)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 2
	}

	// --save persists the resolved settings and optionally continues.
	if opts.save {
		path, perr := defaultConfigPath()
		if perr != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", perr)
			return 2
		}
		if err := saveConfig(path, cfg); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 2
		}
		if cfg.Token != "" && !opts.quiet {
			_, _ = fmt.Fprintf(os.Stderr, "barfi: wrote token to %s (mode 0600)\n", path)
		}
		if opts.file == "" {
			return 0
		}
	}

	if opts.file == "" {
		_, _ = fmt.Fprintln(os.Stderr, "barfi: no file argument (use --help for usage)")
		return 2
	}
	if cfg.Server == "" {
		_, _ = fmt.Fprintln(os.Stderr, "barfi: --server not set (use a flag, BARFI_SERVER, or --save a config)")
		return 2
	}

	f, err := os.Open(opts.file)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 2
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 2
	}

	var partSize int64
	if opts.partSizeStr != "" {
		n, perr := parseSize(opts.partSizeStr)
		if perr != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", perr)
			return 2
		}
		if n < MinPartSize {
			if !opts.quiet {
				_, _ = fmt.Fprintf(os.Stderr, "barfi: --part-size %s below minimum, using %s\n", opts.partSizeStr, humanSize(MinPartSize))
			}
			n = MinPartSize
		}
		if n > MaxPartSize {
			if !opts.quiet {
				_, _ = fmt.Fprintf(os.Stderr, "barfi: --part-size %s above maximum, using %s\n", opts.partSizeStr, humanSize(MaxPartSize))
			}
			n = MaxPartSize
		}
		partSize = n
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fileSize := st.Size()
	fileName := filepathBase(opts.file)
	server := strings.TrimRight(cfg.Server, "/")

	// Resolve final part size for the settings printout.
	effectivePartSize := partSize
	if effectivePartSize == 0 {
		effectivePartSize = calcPartSize(fileSize)
	}
	totalParts := (fileSize + effectivePartSize - 1) / effectivePartSize

	// Print upload settings before starting.
	if !opts.quiet {
		_, _ = fmt.Fprintf(os.Stderr, "file:        %s (%s)\n", fileName, humanSize(fileSize))
		_, _ = fmt.Fprintf(os.Stderr, "server:      %s\n", server)
		_, _ = fmt.Fprintf(os.Stderr, "parts:       %d x %s\n", totalParts, humanSize(effectivePartSize))
		_, _ = fmt.Fprintf(os.Stderr, "workers:     %d\n", cfg.Workers)
		if opts.parentId != "" {
			_, _ = fmt.Fprintf(os.Stderr, "directory:   %s\n", opts.parentId)
		}
		if opts.guestLink != "" {
			_, _ = fmt.Fprintf(os.Stderr, "guest link:  %s\n", opts.guestLink)
		}
		if cfg.LocationId != "" {
			_, _ = fmt.Fprintf(os.Stderr, "location:    %s\n", cfg.LocationId)
		}
	}

	u := &Uploader{
		file:        f,
		fileSize:    fileSize,
		fileName:    fileName,
		server:      server,
		token:       cfg.Token,
		locationId:  cfg.LocationId,
		parentId:    opts.parentId,
		guestLinkId: opts.guestLink,
		note:        opts.note,
		partSize:    partSize,
		workers:     cfg.Workers,
		httpClient:  &http.Client{Timeout: 0},
		progress:    newProgress(opts.quiet, fileName),
	}

	start := time.Now()
	result, err := u.run(ctx)
	if err != nil {
		return formatError(err)
	}

	if !opts.quiet {
		elapsed := time.Since(start)
		var avgSpeed string
		if secs := elapsed.Seconds(); secs > 0 {
			avgSpeed = humanSize(int64(float64(u.fileSize)/secs)) + "/s"
		}
		_, _ = fmt.Fprintf(os.Stderr, "uploaded %s (%s) in %s (%s)\n",
			u.fileName, humanSize(u.fileSize), elapsed.Round(time.Second), avgSpeed)
	}
	if opts.jsonOutput {
		// Pretty-print the raw JSON from the server.
		var buf bytes.Buffer
		if err := json.Indent(&buf, result.rawJSON, "", "  "); err != nil {
			fmt.Println(string(result.rawJSON)) // fall back to raw
		} else {
			fmt.Println(buf.String())
		}
	} else {
		fmt.Println(result.link)
	}
	return 0
}

func runConfig(action string, args []string) int {
	path, err := defaultConfigPath()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 2
	}

	switch action {
	case "show":
		cfg, err := loadConfig(path)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 1
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(data))
		return 0

	case "set":
		if len(args) != 2 {
			_, _ = fmt.Fprintln(os.Stderr, "barfi: --config set requires <key> <value>")
			return 2
		}
		cfg, err := loadConfig(path)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 1
		}
		if err := setConfigField(&cfg, args[0], args[1]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 2
		}
		if err := saveConfig(path, cfg); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 1
		}
		return 0

	case "unset":
		if len(args) != 1 {
			_, _ = fmt.Fprintln(os.Stderr, "barfi: --config unset requires <key>")
			return 2
		}
		cfg, err := loadConfig(path)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 1
		}
		if err := setConfigField(&cfg, args[0], ""); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 2
		}
		if err := saveConfig(path, cfg); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
			return 1
		}
		return 0

	default:
		_, _ = fmt.Fprintf(os.Stderr, "barfi: unknown config action %q (use show, set, or unset)\n", action)
		return 2
	}
}

func setConfigField(cfg *Config, key, value string) error {
	switch key {
	case "server":
		cfg.Server = value
	case "token":
		cfg.Token = value
	case "locationId":
		cfg.LocationId = value
	case "parentId":
		cfg.ParentId = value
	case "workers":
		if value == "" {
			cfg.Workers = 0
			return nil
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("workers must be an integer, got %q", value)
		}
		if n < 1 {
			return fmt.Errorf("workers must be >= 1, got %d", n)
		}
		cfg.Workers = n
	default:
		return fmt.Errorf("unknown config key %q (valid: server, token, locationId, parentId, workers)", key)
	}
	return nil
}

func formatError(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		_, _ = fmt.Fprintln(os.Stderr, "barfi: cancelled")
		return 130
	case errors.Is(err, errExpired):
		_, _ = fmt.Fprintln(os.Stderr, "barfi: upload session expired — start over")
		return 1
	case errors.Is(err, errPartTooLarge):
		_, _ = fmt.Fprintln(os.Stderr, "barfi: part exceeds server size limit (100 MB)")
		return 1
	default:
		_, _ = fmt.Fprintln(os.Stderr, "barfi:", err)
		return 1
	}
}

// filepathBase is a tiny wrapper so we don't import path/filepath for this one use.
func filepathBase(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func main() { os.Exit(runCLI(os.Args[1:])) }
