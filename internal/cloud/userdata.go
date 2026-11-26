package cloud

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	tunnelUser = "chiperka-tunnel"
	// The worker reaches the hub at this address on its own loopback; the hub's
	// reverse tunnel listens there.
	tunnelPort = 18080
)

// provisionScript installs and tunes Docker. It runs both when a server image is
// built and when a worker starts from the stock image (slower, but works without
// a prepared image).
const provisionScript = `#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if ! command -v docker >/dev/null 2>&1; then
  apt-get update -q
  apt-get install -yq docker.io
fi
mkdir -p /etc/docker
if [ ! -f /etc/docker/daemon.json ]; then
  # Many parallel tests each get their own network: widen the address pools.
  cat >/etc/docker/daemon.json <<'JSON'
{"default-address-pools":[{"base":"10.128.0.0/10","size":24}],"log-driver":"local","log-opts":{"max-size":"20m"},"max-concurrent-downloads":10}
JSON
  systemctl restart docker
fi
systemctl enable --now docker
`

// startScript starts the worker on a freshly booted server.
const startScript = `#!/bin/bash
set -euo pipefail
/usr/local/bin/chiperka-provision
# Test containers must not reach the cloud metadata service: the user data holds the worker token.
iptables -C DOCKER-USER -d 169.254.169.254/32 -j REJECT 2>/dev/null || iptables -I DOCKER-USER -d 169.254.169.254/32 -j REJECT
set -a; . /etc/chiperka/worker.env; set +a
docker image inspect "$CHIPERKA_WORKER_IMAGE" >/dev/null 2>&1 || docker pull -q "$CHIPERKA_WORKER_IMAGE"
docker rm -f cloud-worker >/dev/null 2>&1 || true
# The name must not contain "chiperka-": workers up to 2.9 remove such containers on start, themselves included.
docker run -d --name cloud-worker --restart unless-stopped --network host \
  -v /var/run/docker.sock:/var/run/docker.sock --env-file /etc/chiperka/worker.env \
  "$CHIPERKA_WORKER_IMAGE" --concurrency "$CHIPERKA_WORKER_CONCURRENCY" --timeout "$CHIPERKA_WORKER_TIMEOUT" --drain-timeout 10m
# Last-resort safety net: power off even if the hub forgets the server
# (AWS terminates it on shutdown, the janitor deletes it on Hetzner).
shutdown -h "+$CHIPERKA_SELF_DESTRUCT_MIN" >/dev/null 2>&1 || true
if [ "$CHIPERKA_PROVIDER" = aws ]; then
  systemd-run --unit chiperka-spot-watch /usr/local/bin/chiperka-spot-watch
fi
`

// spotWatchScript lets the worker finish what it can when AWS reclaims a spot
// server (2 minutes' notice): SIGTERM stops claiming and drains running tests.
const spotWatchScript = `#!/bin/bash
while sleep 5; do
  t=$(curl -fs -m 2 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300') || continue
  code=$(curl -s -m 2 -o /dev/null -w '%{http_code}' -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/spot/instance-action)
  if [ "$code" = 200 ]; then
    docker stop -t 110 cloud-worker
    exit 0
  fi
done
`

type cloudConfigFile struct {
	Path        string `yaml:"path"`
	Content     string `yaml:"content"`
	Permissions string `yaml:"permissions,omitempty"`
}

type cloudConfigUser struct {
	Name              string   `yaml:"name"`
	Shell             string   `yaml:"shell"`
	LockPasswd        bool     `yaml:"lock_passwd"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
}

type cloudConfig struct {
	SSHDeleteKeys  bool              `yaml:"ssh_deletekeys"`
	SSHGenKeyTypes []string          `yaml:"ssh_genkeytypes"`
	SSHKeys        map[string]string `yaml:"ssh_keys,omitempty"`
	DisableRoot    bool              `yaml:"disable_root"`
	SSHPwauth      bool              `yaml:"ssh_pwauth"`
	Users          []any             `yaml:"users,omitempty"`
	WriteFiles     []cloudConfigFile `yaml:"write_files"`
	RunCmd         []any             `yaml:"runcmd"`
}

func (c *cloudConfig) render() (string, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(b), nil
}

// WorkerUserData is everything a new worker server needs.
type WorkerUserData struct {
	Provider        string
	HostKeyPrivate  string // OpenSSH PEM, pinned by the hub
	HostKeyPublic   string
	HubPublicKey    string
	WorkerImage     string
	WorkerName      string
	WorkerToken     string
	Concurrency     int
	TimeoutSec      int
	SelfDestructMin int
}

func (u WorkerUserData) Render() (string, error) {
	env := map[string]string{
		"CHIPERKA_API_URL":            fmt.Sprintf("http://127.0.0.1:%d", tunnelPort),
		"CHIPERKA_WORKER_TOKEN":       u.WorkerToken,
		"CHIPERKA_WORKER_NAME":        u.WorkerName,
		"CHIPERKA_WORKER_IMAGE":       u.WorkerImage,
		"CHIPERKA_WORKER_CONCURRENCY": fmt.Sprint(u.Concurrency),
		"CHIPERKA_WORKER_TIMEOUT":     fmt.Sprint(u.TimeoutSec),
		"CHIPERKA_REGISTER_TIMEOUT":   "10m", // the tunnel comes up a little after boot
		"CHIPERKA_SELF_DESTRUCT_MIN":  fmt.Sprint(u.SelfDestructMin),
		"CHIPERKA_PROVIDER":           u.Provider,
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var envFile strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&envFile, "%s=%s\n", k, env[k])
	}

	// The hub's key may only open the one reverse tunnel: no shell, no other forwards.
	authorized := fmt.Sprintf(`restrict,port-forwarding,permitlisten="127.0.0.1:%d",command="/usr/sbin/nologin" %s`, tunnelPort, u.HubPublicKey)
	cc := cloudConfig{
		SSHDeleteKeys:  true,
		SSHGenKeyTypes: []string{},
		SSHKeys:        map[string]string{"ed25519_private": u.HostKeyPrivate, "ed25519_public": u.HostKeyPublic},
		DisableRoot:    true,
		Users: []any{"default", cloudConfigUser{
			Name: tunnelUser, Shell: "/usr/sbin/nologin", LockPasswd: true, SSHAuthorizedKeys: []string{authorized},
		}},
		WriteFiles: []cloudConfigFile{
			{Path: "/etc/ssh/sshd_config.d/10-chiperka.conf", Permissions: "0644", Content: "PasswordAuthentication no\nPermitRootLogin no\nAllowUsers " + tunnelUser + "\nAllowTcpForwarding remote\nGatewayPorts no\nX11Forwarding no\nClientAliveInterval 15\n"},
			{Path: "/etc/chiperka/worker.env", Permissions: "0600", Content: envFile.String()},
			{Path: "/usr/local/bin/chiperka-provision", Permissions: "0755", Content: provisionScript},
			{Path: "/usr/local/bin/chiperka-start", Permissions: "0755", Content: startScript},
			{Path: "/usr/local/bin/chiperka-spot-watch", Permissions: "0755", Content: spotWatchScript},
		},
		RunCmd: []any{
			[]string{"systemctl", "restart", "ssh"},
			[]string{"/usr/local/bin/chiperka-start"},
		},
	}
	return cc.render()
}

// ImageUserData provisions a builder server, pre-pulls images, cleans up and
// powers off; the builder then snapshots the stopped server.
type ImageUserData struct {
	WorkerImage      string
	Images           []string // test and helper images to bake in
	Registry         string   // optional login for private images; removed before the snapshot
	RegistryUser     string
	RegistryPassword string
}

func (u ImageUserData) Render() (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -uo pipefail\n/usr/local/bin/chiperka-provision\n")
	if u.Registry != "" {
		fmt.Fprintf(&b, "printf '%%s' %s | docker login -u %s --password-stdin %s\n", shellQuote(u.RegistryPassword), shellQuote(u.RegistryUser), shellQuote(u.Registry))
	}
	for _, img := range append([]string{u.WorkerImage}, u.Images...) {
		fmt.Fprintf(&b, "docker pull -q %s || echo 'warning: could not pull %s'\n", shellQuote(img), img)
	}
	b.WriteString(`docker logout >/dev/null 2>&1 || true
rm -rf /root/.docker
# This script holds the registry password: it must not end up in the image.
rm -f "$0"
apt-get clean
# Clones must boot like new machines: fresh cloud-init run, machine ID and host keys.
cloud-init clean --logs --machine-id --seed
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
poweroff
`)
	cc := cloudConfig{
		DisableRoot: true,
		WriteFiles: []cloudConfigFile{
			{Path: "/usr/local/bin/chiperka-provision", Permissions: "0755", Content: provisionScript},
			{Path: "/usr/local/sbin/chiperka-build-image", Permissions: "0700", Content: b.String()},
		},
		RunCmd: []any{[]string{"/usr/local/sbin/chiperka-build-image"}},
	}
	return cc.render()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
