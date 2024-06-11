package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	zl "github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var endpoint = "https://hidan.co"

var (
	token          = flag.String("token", "", "auth token of account")
	directoryId    = flag.String("dir", "", "id of directory where file will be uploaded")
	showVersion    = flag.Bool("version", false, "print version information and exit")
	debugMode      = flag.Bool("debug", false, "enable debug logs")
	silentMode     = flag.Bool("silent", false, "do not print any logs on screen")
	customEndpoint = flag.String("endpoint", "", "override default endpoint - for testing propose")
)

func main() {

	flag.Parse()

	if *showVersion {
		fmt.Printf("barfi: %s\n", Version())
		os.Exit(0)
	}

	// Setup logger
	log.Logger = zl.New(zl.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).With().Timestamp().Logger()
	zl.SetGlobalLevel(zl.InfoLevel)
	if *debugMode {
		zl.SetGlobalLevel(zl.DebugLevel)
	}

	if *silentMode {
		zl.SetGlobalLevel(zl.ErrorLevel)
	}

	if *customEndpoint != "" {
		endpoint = *customEndpoint
	}

	if len(flag.Args()) < 1 {
		log.Error().Msg("file path is required")
		os.Exit(1)
	}

	filePath := flag.Arg(0)

	file, err := os.Open(filePath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open file")
	}
	defer file.Close()

	log.Info().
		Bool("silent", *silentMode).
		Bool("debug", *debugMode).
		Str("token", *token).
		Str("directoryId", *directoryId).
		Str("version", Version()).
		Str("endpoint", endpoint).
		Msg("starting uploader")

	mu, err := NewUploader(file, directoryId, token)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to init upload")
	}

	err = mu.Upload()
	if err != nil {
		log.Fatal().Err(err).Msg("upload failed with error")
	}
}
