package assets

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rootlessAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := Rootless.ReadFile("rootless/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The fixture substitutes kernel/image paths only in a temporary copy. Production
// launchers deliberately expose no environment overrides for these security inputs.
func TestRootlessLauncherPrerequisites(t *testing.T) {
	for _, test := range []struct{ name, fault, want string }{
		{"root user", "root", "requires a non-root user"},
		{"missing tool", "tool", "missing prerequisite: slirp4netns"},
		{"missing range", "range", "subordinate ID range"},
		{"missing TUN device", "tun", "missing prerequisite: /dev/net/tun character device"},
		{"regular file instead of TUN", "tun-file", "missing prerequisite: /dev/net/tun character device"},
		{"userns denied", "userns", "unprivileged user namespaces are not enabled"},
		{"mapping denied", "mapping", "UID/GID mapping failed"},
		{"config rejected", "config", "invalid generated dockerd configuration"},
		{"bad host gateway", "gateway", "no usable IPv4 host-gateway"},
		{"unspecified host gateway", "unspecified", "no usable IPv4 host-gateway"},
		{"malformed host gateway", "malformed", "no usable IPv4 host-gateway"},
		{"missing host gateway", "missing-gateway", "no usable IPv4 host-gateway"},
		{"runtime gateway in zero prefix", "zero-prefix", ""},
		{"missing delegation", "cgroup", "resource limits require cgroup-v2 user delegation"},
		{"valid launch", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			runtimeDir := filepath.Join(dir, "run")
			for _, path := range []string{bin, runtimeDir, filepath.Join(dir, ".local/share/docker")} {
				if err := os.MkdirAll(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			write("id", `if [ "$1" = -un ]; then echo rootless; elif [ "$FAULT" = root ]; then echo 0; else echo 1000; fi`)
			write("slirp4netns", "exit 0")
			write("newuidmap", "exit 0")
			write("newgidmap", "exit 0")
			write("docker-init", "exit 0")
			write("unshare", `[ "$FAULT" != userns ]`)
			write("rootlesskit", `printf '%s\n' "$@" >> "$CALLS"; [ "$FAULT" != mapping ]`)
			write("dockerd", `[ "$FAULT" != config ]`)
			write("iptables", "exit 0")
			write("ip", "echo 'default via 10.222.0.53 dev eth0'")
			script := rootlessAsset(t, "launch.sh")
			// /dev/null supplies a real, openable character device for the fixture.
			// Only the kernel path is substituted; keep diagnostics unchanged.
			tun := "/dev/null"
			if test.fault == "tun" || test.fault == "tun-file" {
				tun = filepath.Join(dir, "tun")
				if test.fault == "tun-file" {
					if err := os.WriteFile(tun, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			script = strings.ReplaceAll(script, "[ -c /dev/net/tun ]", "[ -c "+tun+" ]")
			script = strings.ReplaceAll(script, "9<>/dev/net/tun", "9<>"+tun)

			script = strings.ReplaceAll(script, "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "export PATH="+bin+":/usr/bin:/bin")
			script = strings.ReplaceAll(script, "export XDG_RUNTIME_DIR=/run/user/$uid", "export XDG_RUNTIME_DIR="+runtimeDir)
			for _, name := range []string{"subuid", "subgid"} {
				path := filepath.Join(dir, name)
				data := "rootless:100000:65536\n"
				if test.fault == "range" {
					data = "rootless:100000:1\n"
				}
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				script = strings.ReplaceAll(script, "/etc/"+name, path)
			}
			hosts := filepath.Join(dir, "hosts")
			hostIP := "172.17.0.1"
			switch test.fault {
			case "gateway":
				hostIP = "127.0.0.1"
			case "unspecified":
				hostIP = "0.0.0.0"
			case "malformed":
				hostIP = "256.1.2.3"
			case "missing-gateway":
				hostIP = ""
			case "zero-prefix":
				hostIP = "0.250.250.254"
			}
			if err := os.WriteFile(hosts, []byte(hostIP+" host.internal\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script = strings.ReplaceAll(script, "/etc/hosts", hosts)
			template := filepath.Join(dir, "template.json")
			if err := os.WriteFile(template, []byte(`{"host-gateway-ips":["HOLE_HOST_GATEWAY_IP"]}`), 0600); err != nil {
				t.Fatal(err)
			}
			script = strings.ReplaceAll(script, "/etc/hole/daemon.json", template)
			if test.fault == "tool" {
				// Use an impossible name, independent of tools installed on the test machine.
				script = strings.ReplaceAll(script, "command -v \"$tool\"", `command -v "$(if [ "$tool" = slirp4netns ]; then echo hole-no-such-slirp-binary; else echo "$tool"; fi)"`)
			}
			requireCgroup := "0"
			if test.fault == "cgroup" {
				requireCgroup = "1"
				script = strings.ReplaceAll(script, "/sys/fs/cgroup/", filepath.Join(dir, "missing-cgroup")+"/")
			}
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+dir, "FAULT="+test.fault, "CALLS="+filepath.Join(dir, "calls"), "HOLE_GATEWAY_IP=10.222.0.53", "HOLE_REQUIRE_CGROUP="+requireCgroup, "PATH=/hostile")
			out, err := cmd.CombinedOutput()
			if test.want != "" {
				if err == nil || !strings.Contains(string(out), test.want) {
					t.Fatalf("wanted %q: err=%v output=%s", test.want, err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid launch: %v\n%s", err, out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			for _, arg := range []string{"--net=slirp4netns", "--mtu=1500", "--disable-host-loopback", "--copy-up=/run", "--port-driver=builtin", "0.0.0.0:2375:2375/tcp", "docker-init"} {
				if !strings.Contains(string(calls), arg+"\n") {
					t.Errorf("launch missing %s: %s", arg, calls)
				}
			}
			config, _ := os.ReadFile(filepath.Join(runtimeDir, "daemon.json"))
			if !strings.Contains(string(config), hostIP) || strings.Contains(string(config), "HOLE_HOST") {
				t.Fatalf("host gateway substitution: %s", config)
			}
		})
	}
}

func TestRootlessDaemonPinsPATHAndConfig(t *testing.T) {
	for _, name := range []string{"launch.sh", "daemon.sh", "health.sh", "hole-dockerd-netns"} {
		script := rootlessAsset(t, name)
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s syntax: %v %s", name, err, out)
		}
		if !strings.Contains(script, "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n") {
			t.Fatalf("%s inherits caller PATH", name)
		}
	}
	script := rootlessAsset(t, "daemon.sh")
	if !strings.Contains(script, `exec dockerd --config-file="$XDG_RUNTIME_DIR/daemon.json"`) || strings.Contains(script, `"$@"`) {
		t.Fatal("daemon command can override config")
	}
}

func TestRootlessNamespaceResolver(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/net"); err != nil {
		t.Skip("Linux namespace fixture required")
	}
	for _, mode := range []string{"missing", "ambiguous", "launcher", "daemon"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			proc := filepath.Join(dir, "proc")
			makeProc := func(pid, comm, netns string) {
				path := filepath.Join(proc, pid)
				if err := os.MkdirAll(filepath.Join(path, "ns"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "comm"), []byte(comm+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(netns, filepath.Join(path, "ns/net")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/proc/self/ns/user", filepath.Join(path, "ns/user")); err != nil {
					t.Fatal(err)
				}
			}
			// Use another real namespace as the fake launcher's netns, so real namespace
			// FDs can be opened without needing userns permission in the test runner.
			makeProc("1", "rootlesskit", "/proc/self/ns/mnt")
			if mode != "missing" {
				ns := "/proc/self/ns/net"
				if mode == "launcher" {
					ns = "/proc/self/ns/net"
				}
				makeProc("42", "dockerd", ns)
			}
			if mode == "ambiguous" {
				makeProc("43", "dockerd", "/proc/self/ns/net")
			}
			script := rootlessAsset(t, "hole-dockerd-netns")
			script = strings.NewReplacer("/proc/[0-9]", proc+"/[0-9]", "${comm#/proc/}", "${comm#"+proc+"/}", "/proc/$found/", proc+"/$found/", "/proc/1/", proc+"/1/").Replace(script)
			if mode == "launcher" {
				script = strings.ReplaceAll(script, proc+"/1/ns/net", "/proc/self/ns/net")
			}
			cmd := exec.Command("sh", "-c", script)
			out, err := cmd.CombinedOutput()
			if mode == "daemon" {
				if err != nil || !strings.Contains(string(out), "pid=42 netns=net:[") {
					t.Fatalf("wrong daemon namespace: %v %s", err, out)
				}
			} else if err == nil {
				t.Fatalf("unsafe %s namespace accepted: %s", mode, out)
			}
		})
	}
}
