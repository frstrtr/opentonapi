package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"

	"github.com/caarlos0/env/v6"
	"github.com/tonkeeper/tongo"
	"github.com/tonkeeper/tongo/config"
)

type Config struct {
	API struct {
		Port        int      `env:"PORT" envDefault:"8081"`
		UnixSockets []string `env:"UNIX_SOCKETS" envSeparator:","`
	}
	App struct {
		LogLevel           string              `env:"LOG_LEVEL" envDefault:"INFO"`
		MetricsPort        int                 `env:"METRICS_PORT" envDefault:"9010"`
		Accounts           accountsList        `env:"ACCOUNTS" envDefault:"0:0e41dc1dc3c9067ed24248580e12b3359818d83dee0304fabcf80845eafafdb2"`
		LiteServers        []config.LiteServer `env:"LITE_SERVERS"`
		SendingLiteservers []config.LiteServer `env:"SENDING_LITE_SERVERS"`
		IsTestnet          bool                `env:"IS_TESTNET" envDefault:"false"`
		AccountsFile       string              `env:"ACCOUNTS_FILE" envDefault:"accounts.txt"`
	}
	TonConnect struct {
		Secret string `env:"TON_CONNECT_SECRET"`
	}
}

type accountsList []tongo.AccountID

const (
	AddressPath    = "https://raw.githubusercontent.com/tonkeeper/ton-assets/main/accounts.json"
	CollectionPath = "https://raw.githubusercontent.com/tonkeeper/ton-assets/main/collections.json"
	JettonPath     = "https://raw.githubusercontent.com/tonkeeper/ton-assets/main/jettons.json"
)

func Load() Config {
	var c Config

	//    if err := env.Parse(&c); err != nil {
	//	log.Panicf("[‼ Config parsing failed] %+v\n", err)
	//    }

	if err := env.ParseWithFuncs(&c, map[reflect.Type]env.ParserFunc{
		reflect.TypeOf([]config.LiteServer{}): func(v string) (interface{}, error) {
			servers, err := config.ParseLiteServersEnvVar(v)
			if err != nil {
				log.Printf("SERVERS: %v", servers)
				return nil, err
			}
			if len(servers) == 0 {
				return nil, fmt.Errorf("empty liteservers list")
			}
			log.Printf("SERVERS: %v", servers)
			return servers, nil
		},
		reflect.TypeOf(accountsList{}): func(v string) (interface{}, error) {
			log.Printf("Fallback: Load accounts from environment variable")
			log.Printf("ACCOUNTS: %v", v)
			var fallbackAccs accountsList
			for _, s := range strings.Split(v, ",") {
				log.Printf("Iterating over accounts: %v", s)
				account, err := tongo.ParseAddress(s)
				if err != nil {
					return nil, err
				}
				fallbackAccs = append(fallbackAccs, account.ID)
			}
			return fallbackAccs, nil
		},
	}); err != nil {
		log.Panicf("[‼️ Config parsing failed] %+v\n", err)
	}

	// Handle accounts loading after other config parsing
	accs, err := parseAccountsFromFile(c.App.AccountsFile)
	if err == nil {
		c.App.Accounts = accs.(accountsList)
	} else {
		// If loading from file fails, the ACCOUNTS env var will be used
		// since it's already parsed into c.App.Accounts
		log.Printf("Failed to load accounts from file: %v", err)
	}
	return c
}

func parseAccountsFromFile(v string) (interface{}, error) {
	var accs accountsList

	// Check if the environment variable is set
	if v == "" {
		log.Printf("INFO: Environment variable not set, using default accounts file 'accounts.txt'")
		v = "accounts.txt" // Use the default file name
	}

	file, err := os.Open(v)
	if err != nil {
		return nil, fmt.Errorf("error opening accounts file '%s': %v", v, err)
	}
	defer file.Close()

	// 1. Count Total Lines (Estimate Progress)
	scanner := bufio.NewScanner(file)
	totalLines := 0
	for scanner.Scan() {
		totalLines++
	}
	file.Seek(0, 0) // Reset file pointer to the beginning

	// 2. Read Accounts and Print Progress
	log.Printf("Loading accounts from file '%s'...", v)

	scanner = bufio.NewScanner(file)
	currentLine := 0
	for scanner.Scan() {
		line := scanner.Text()
		// 1. Split line to remove any text after comma
		parts := strings.Split(line, ",")
		if len(parts) > 0 {
			line = parts[0] // Take only the first part (before the comma)
		}

		account, err := tongo.ParseAddress(line)
		if err != nil {
			log.Printf("WARNING: Skipping invalid account: %v", err)
			continue
		}
		accs = append(accs, account.ID)

		// 3. Update Progress (Simple Text-Based Output)
		currentLine++
		progressPercentage := int((float64(currentLine) / float64(totalLines)) * 100)
		if progressPercentage%10 == 0 { // Update every 10%
			fmt.Printf("\rProgress: %d%%  ", progressPercentage)
		}
	}

	log.Printf("Finished loading %d accounts from file '%s'", currentLine, v) // Use currentLine
	log.Printf("Subscribing to the loaded accounts:")
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading accounts file '%s': %v", v, err)
	}

	return accs, nil
}
