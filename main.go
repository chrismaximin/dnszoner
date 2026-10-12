// dnszoner is a terminal editor for DNS zones across providers.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/changelog"
	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/fake"
	"github.com/chrismaximin/dnszoner/internal/store"
	"github.com/chrismaximin/dnszoner/internal/ui"
)

// version is set by release builds; go install builds use the module version.
var version = ""

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dnszoner:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", config.DefaultPath(), "config file listing provider accounts")
		dbPath     = flag.String("db", "", "cache database (default: in the user cache dir)")
		useFake    = flag.Bool("fake", false, "use a separate database filled with made-up sources, domains and records")
		fakeReset  = flag.Bool("fake-reset", false, "with --fake, regenerate the fake data")
		noSync     = flag.Bool("no-sync", false, "don't refresh from providers on startup (ctrl+r syncs)")
		logPath    = flag.String("log", "", "file every record change is logged to (default: "+changelog.DefaultPath(false)+")")
		showVer    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: dnszoner [flags]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nConfig file: %s (edit accounts in the app with ctrl+o)\n", config.DefaultPath())
	}
	flag.Parse()
	if *showVer {
		fmt.Println("dnszoner", versionString())
		return nil
	}

	if *dbPath == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		name := "cache.db"
		if *useFake {
			name = "fake.db"
		}
		*dbPath = filepath.Join(dir, "dnszoner", name)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if *logPath == "" {
		*logPath = changelog.DefaultPath(*useFake)
	}
	changes, err := changelog.Open(*logPath)
	if err != nil {
		return fmt.Errorf("opening the change log: %w", err)
	}
	defer changes.Close()

	providers := map[string]dns.Provider{}
	syncOnStart := !*noSync
	cfg := &config.Config{}
	if *useFake {
		empty, err := st.Empty()
		if err != nil {
			return err
		}
		if empty || *fakeReset {
			if err := fake.Seed(st); err != nil {
				return fmt.Errorf("seeding fake data: %w", err)
			}
		}
		for _, src := range fake.Sources {
			providers[src.Name] = &dns.Fake{Store: st, Source: src.Name}
			cfg.Sources = append(cfg.Sources, fake.ConfigSource(src))
		}
		syncOnStart = false
	} else {
		if cfg, err = config.Load(*configPath); err != nil {
			return err
		}
		var sources []store.Source
		for _, src := range cfg.Sources {
			sources = append(sources, store.Source{Name: src.Name, Provider: src.Provider})
			providers[src.Name] = dns.New(src)
		}
		if err := st.SetSources(sources); err != nil {
			return err
		}
	}

	m := ui.New(ui.Options{
		Store: st, Providers: providers, SyncOnStart: syncOnStart, Fake: *useFake,
		Config: cfg, ConfigPath: *configPath, ChangeLog: changes,
	})
	_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
