package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sbraznikov/istari/internal/api"
	"github.com/sbraznikov/istari/internal/config"
)

func newDaemonCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the istari daemon in the foreground",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(opts.Config)
			if err != nil {
				return err
			}
			dataDir, err := cfg.Daemon.DataDirPath()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			return api.RunDaemon(ctx, dataDir)
		},
	}
	cmd.AddCommand(newDaemonInstallCommand(opts))
	return cmd
}

// loadConfig loads the config at path. Without an explicit path a missing
// ~/.config/istari/config.toml means an installation with the defaults.
func loadConfig(path string) (*config.Config, error) {
	explicit := path != ""
	if !explicit {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load(path)
	if !explicit && errors.Is(err, fs.ErrNotExist) {
		def := config.Default()
		return &def, nil
	}
	return cfg, err
}

func newDaemonInstallCommand(opts *Options) *cobra.Command {
	kind := map[string]string{"darwin": "launchd"}[runtime.GOOS]
	if kind == "" {
		kind = "systemd"
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write a launchd or systemd unit for the daemon; enabling it is up to you",
		Args:  Args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			args := []string{exe, "daemon"}
			if opts.Config != "" {
				cfgPath, err := filepath.Abs(opts.Config)
				if err != nil {
					return err
				}
				args = append(args, "--config", cfgPath)
			}
			var path, unit, enable string
			switch kind {
			case "launchd":
				path = filepath.Join(home, "Library", "LaunchAgents", "dev.istari.daemon.plist")
				unit = launchdPlist(args, filepath.Join(home, "Library", "Logs", "istari.log"))
				enable = "launchctl bootstrap gui/$(id -u) " + path
			case "systemd":
				path = filepath.Join(home, ".config", "systemd", "user", "istari.service")
				unit = systemdUnit(args)
				enable = "systemctl --user enable --now istari.service"
			default:
				return usageError{fmt.Errorf("--kind %q is not launchd or systemd", kind)}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\nIt is not enabled. To enable and start it, run:\n  %s\n", path, enable)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", kind, "unit kind: launchd or systemd")
	return cmd
}

func launchdPlist(args []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.istari.daemon</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	fmt.Fprintf(&b, `  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(logPath))
	return b.String()
}

func systemdUnit(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
	}
	return fmt.Sprintf(`[Unit]
Description=istari daemon

[Service]
ExecStart=%s
Restart=on-failure

[Install]
WantedBy=default.target
`, strings.Join(quoted, " "))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
