// Package setup implements `stretchy --init`: it installs the running
// binary to /sbin/stretchy, creates the system user, directories,
// default configuration, a systemd unit and a logrotate rule.
package setup

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
)

const (
	binPath     = "/sbin/stretchy"
	configDir   = "/etc/stretchy"
	configPath  = "/etc/stretchy/config.yaml"
	dataDir     = "/var/lib/stretchy"
	logDir      = "/var/log/stretchy"
	unitPath    = "/etc/systemd/system/stretchy.service"
	logrotPath  = "/etc/logrotate.d/stretchy"
	serviceUser = "stretchy"
)

const defaultConfig = `# stretchy configuration
# Elasticsearch-compatible mini search engine.

server:
  host: 127.0.0.1
  port: 9200

# Uncomment to require HTTP basic auth (recommended when the port is
# reachable from other hosts).
#auth:
#  username: elastic
#  password: change-me

storage:
  data_dir: /var/lib/stretchy

logging:
  dir: /var/log/stretchy
  level: info
`

const systemdUnit = `[Unit]
Description=stretchy - Elasticsearch-compatible mini search engine
Documentation=https://github.com/ostap-mykhaylyak/stretchy
After=network.target

[Service]
Type=simple
User=stretchy
Group=stretchy
ExecStart=/sbin/stretchy --config /etc/stretchy/config.yaml
Restart=on-failure
RestartSec=5
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/stretchy /var/log/stretchy

[Install]
WantedBy=multi-user.target
`

const logrotateRule = `/var/log/stretchy/*.log {
    weekly
    rotate 8
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
`

func Install(version string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("--init is only supported on Linux")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("--init must run as root (try: sudo stretchy --init)")
	}

	fmt.Printf("stretchy %s installer\n\n", version)

	// 1. system user
	if _, err := user.Lookup(serviceUser); err != nil {
		step("creating system user " + serviceUser)
		cmd := exec.Command("useradd", "--system", "--no-create-home",
			"--home-dir", dataDir, "--shell", "/usr/sbin/nologin", serviceUser)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("useradd: %v: %s", err, out)
		}
	} else {
		step("system user " + serviceUser + " already exists")
	}

	// 2. binary
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	self, _ = filepath.EvalSymlinks(self)
	if self == binPath {
		step("binary already running from " + binPath)
	} else {
		step("installing binary to " + binPath)
		if err := copyFile(self, binPath, 0o755); err != nil {
			return err
		}
	}

	// 3. directories
	for _, dir := range []string{configDir, dataDir, logDir} {
		step("ensuring directory " + dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := chownRecursive(dataDir, serviceUser); err != nil {
		return err
	}
	if err := chownRecursive(logDir, serviceUser); err != nil {
		return err
	}

	// 4. default config (never overwrite an existing one)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		step("writing default config to " + configPath)
		if err := os.WriteFile(configPath, []byte(defaultConfig), 0o644); err != nil {
			return err
		}
	} else {
		step("config " + configPath + " already exists, leaving it untouched")
	}

	// 5. systemd unit
	step("writing systemd unit " + unitPath)
	if err := os.WriteFile(unitPath, []byte(systemdUnit), 0o644); err != nil {
		return err
	}

	// 6. logrotate
	step("writing logrotate rule " + logrotPath)
	if err := os.WriteFile(logrotPath, []byte(logrotateRule), 0o644); err != nil {
		return err
	}

	// 7. systemd reload + enable
	step("reloading systemd")
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	step("enabling stretchy.service")
	if out, err := exec.Command("systemctl", "enable", "stretchy.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %v: %s", err, out)
	}

	fmt.Println("\nInstallation complete. Start the service with:")
	fmt.Println("  sudo systemctl start stretchy")
	fmt.Println("Then check:")
	fmt.Println("  curl http://127.0.0.1:9200/")
	return nil
}

func step(msg string) {
	fmt.Println("  • " + msg)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// write to a temp file first so a running binary is replaced
	// atomically (and ETXTBSY is avoided)
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func chownRecursive(root, username string) error {
	out, err := exec.Command("chown", "-R", username+":"+username, root).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown %s: %v: %s", root, err, out)
	}
	return nil
}
