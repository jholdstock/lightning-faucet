package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/btcsuite/go-flags"
	"github.com/decred/dcrd/dcrutil"
)

const (
	defaultTLSCertFilename  = "tls.cert"
	defaultMacaroonFilename = "admin.macaroon"
	defaultLogFilename      = "dcrlnfaucet.log"
	defaultConfigFilename   = "dcrlnfaucet.conf"
	defaultLogLevel         = "info"
	defaultLndIP            = "10.0.0.9"
	defaultNetParams        = "testnet"
	defaultLndNodes         = "localhost:10009"
	defaultBindAddr         = ":80"
	defaultUseLeHTTPS       = false
	defaultWipeChannels     = false
	defaultDomain           = "faucet.lightning.community"
	defaultNetwork          = "decred"
)

var (
	lndHomeDir          = dcrutil.AppDataDir("dcrlnd", false)
	tlsCertPath         = filepath.Join(lndHomeDir, defaultTLSCertFilename)
	defaultMacaroonPath = filepath.Join(
		lndHomeDir, "data", "chain", "decred", "testnet",
		defaultMacaroonFilename,
	)
	defaultDataDir = dcrutil.AppDataDir("dcrlnfaucet", false)
	defaultLogPath = filepath.Join(
		defaultDataDir, "logs", "decred", "testnet",
		defaultLogFilename,
	)
	defaultConfigFile = filepath.Join(
		defaultDataDir, defaultConfigFilename,
	)
)

type config struct {
	LndIP        string `long:"lnd_ip" description:"the public IP address of the faucet's node"`
	NetParams    string `long:"net" description:"decred network to operate on"`
	LndNodes     string `long:"nodes" description:"comma separated list of host:port"`
	BindAddr     string `long:"bind_addr" description:"port to listen for http"`
	UseLeHTTPS   bool   `long:"use_le_https" description:"use https via lets encrypt"`
	WipeChannels bool   `long:"wipe_chans" description:"close all faucet channels and exit"`
	Domain       string `long:"domain" description:"the domain of the faucet, required for TLS"`
	Network      string `long:"network" description:"the network of the faucet"`
}

func loadConfig() (*config, []string, error) {
	// Default config.
	cfg := config{
		LndIP:        defaultLndIP,
		NetParams:    defaultNetParams,
		LndNodes:     defaultLndNodes,
		BindAddr:     defaultBindAddr,
		UseLeHTTPS:   defaultUseLeHTTPS,
		WipeChannels: defaultWipeChannels,
		Domain:       defaultDomain,
		Network:      defaultNetwork,
	}

	// Pre-parse the command line options to see if an alternative config
	// file was specified.  Any errors aside from the
	// help message error can be ignored here since they will be caught by
	// the final parse below.
	preCfg := cfg
	preParser := flags.NewParser(&preCfg, flags.HelpFlag)
	_, err := preParser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
			fmt.Fprintln(os.Stderr, err)
			return nil, nil, err
		}
	}

	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	usageMessage := fmt.Sprintf("Use %s -h to show usage", appName)

	// Load additional config from file.
	var configFileError error
	parser := flags.NewParser(&cfg, flags.Default)

	err = flags.NewIniParser(parser).ParseFile(defaultConfigFile)
	if err != nil {
		if _, ok := err.(*os.PathError); !ok {
			fmt.Fprintf(os.Stderr, "Error parsing config "+
				"file: %v\n", err)
			fmt.Fprintln(os.Stderr, usageMessage)
			return nil, nil, err
		}
		configFileError = err
	}

	// Parse command line options again to ensure they take precedence.
	remainingArgs, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); !ok || e.Type != flags.ErrHelp {
			fmt.Fprintln(os.Stderr, usageMessage)
		}
		return nil, nil, err
	}

	// Create the home directory if it doesn't already exist.
	funcName := "loadConfig"
	err = os.MkdirAll(defaultDataDir, 0700)
	if err != nil {
		// Show a nicer error message if it's because a symlink is
		// linked to a directory that does not exist (probably because
		// it's not mounted).
		if e, ok := err.(*os.PathError); ok && os.IsExist(err) {
			if link, lerr := os.Readlink(e.Path); lerr == nil {
				str := "is symlink %s -> %s mounted?"
				err = fmt.Errorf(str, e.Path, link)
			}
		}

		str := "%s: Failed to create home directory: %v"
		err := fmt.Errorf(str, funcName, err)
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, err
	}

	// Initialize log rotation.  After log rotation has been initialized, the
	// logger variables may be used.
	initLogRotator(defaultLogPath)
	setLogLevels(defaultLogLevel)

	// Warn about missing config file only after all other configuration is
	// done.  This prevents the warning on help messages and invalid
	// options.  Note this should go directly before the return.
	if configFileError != nil {
		log.Warnf("%v", configFileError)
	}

	return &cfg, remainingArgs, nil
}
