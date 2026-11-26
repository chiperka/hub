package cloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DockerDev is a development stand-in for a cloud provider: every "server" is a
// worker container on the local Docker daemon that talks to the hub directly
// (no SSH tunnel). It lets you try the autoscaler, the UI and the worker
// lifecycle without cloud credentials. Enable with CHIPERKA_CLOUD_DEV_DOCKER=hetzner|aws.
type DockerDev struct {
	As     string // provider it impersonates: "hetzner" or "aws"
	HubURL string // how containers reach the hub, e.g. http://host.docker.internal:8080
}

func (d *DockerDev) Name() string { return d.As }
func (d *DockerDev) Currency() string {
	if d.As == "aws" {
		return "USD"
	}
	return "EUR"
}

func (d *DockerDev) docker(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (d *DockerDev) Check(ctx context.Context) error {
	_, err := d.docker(ctx, "version", "--format", "{{.Server.Version}}")
	return err
}

func (d *DockerDev) Prepare(context.Context, PrepareRequest) error { return nil }

// workerEnv pulls the worker's environment file out of the cloud-init user data.
func workerEnv(userData string) (map[string]string, error) {
	var cc struct {
		WriteFiles []cloudConfigFile `yaml:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(userData), &cc); err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, f := range cc.WriteFiles {
		if f.Path != "/etc/chiperka/worker.env" {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(f.Content))
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), "="); ok {
				env[k] = v
			}
		}
	}
	if env["CHIPERKA_WORKER_TOKEN"] == "" {
		return nil, fmt.Errorf("user data has no worker environment")
	}
	return env, nil
}

func (d *DockerDev) Create(ctx context.Context, r CreateRequest) (*Instance, error) {
	env, err := workerEnv(r.UserData)
	if err != nil {
		return nil, err
	}
	args := []string{"run", "-d", "--name", "cloud-dev-" + r.Name, // not "chiperka-…": the worker would remove it
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"--add-host", "host.docker.internal:host-gateway",
		"--label", LabelManaged + "=true", "--label", "chiperka-name=" + r.Name,
		"--label", "chiperka-type=" + r.Type, "--label", "chiperka-location=" + r.Location,
		"-e", "CHIPERKA_API_URL=" + d.HubURL,
		"-e", "CHIPERKA_WORKER_TOKEN=" + env["CHIPERKA_WORKER_TOKEN"],
		"-e", "CHIPERKA_WORKER_NAME=" + env["CHIPERKA_WORKER_NAME"],
	}
	for k, v := range r.Labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, env["CHIPERKA_WORKER_IMAGE"], "--concurrency", env["CHIPERKA_WORKER_CONCURRENCY"], "--timeout", env["CHIPERKA_WORKER_TIMEOUT"])
	created := time.Now() // before the run: the worker may sign in before docker returns
	id, err := d.docker(ctx, args...)
	if err != nil {
		return nil, err
	}
	return &Instance{ID: id[:12], Name: r.Name, Type: r.Type, Location: r.Location, State: StateRunning, CreatedAt: created, Labels: r.Labels}, nil
}

func (d *DockerDev) Delete(ctx context.Context, id string) error {
	_, err := d.docker(ctx, "rm", "-f", id)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

func (d *DockerDev) List(ctx context.Context) ([]Instance, error) {
	out, err := d.docker(ctx, "ps", "-a", "--no-trunc", "--filter", "label="+LabelManaged+"=true", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var res []Instance
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		var c struct {
			ID, State, Labels, CreatedAt string
		}
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		labels := map[string]string{}
		for _, kv := range strings.Split(c.Labels, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				labels[k] = v
			}
		}
		in := Instance{ID: c.ID[:12], Name: labels["chiperka-name"], Type: labels["chiperka-type"], Location: labels["chiperka-location"], Labels: labels}
		in.CreatedAt, _ = time.Parse("2006-01-02 15:04:05 -0700 MST", c.CreatedAt)
		switch c.State {
		case "running":
			in.State = StateRunning
		case "created", "restarting":
			in.State = StatePending
		default:
			in.State = StateStopped
		}
		res = append(res, in)
	}
	return res, nil
}

func (d *DockerDev) HourlyPrice(context.Context, string, string, bool) (float64, error) {
	if d.As == "aws" {
		return 0.39, nil
	}
	return 0.208, nil
}

func (d *DockerDev) ValidateType(context.Context, string) error  { return nil }
func (d *DockerDev) BaseImage(context.Context) (string, error)   { return "local", nil }
func (d *DockerDev) LatestImage(context.Context) (*Image, error) { return nil, nil }
func (d *DockerDev) ListImages(context.Context) ([]Image, error) { return nil, nil }
func (d *DockerDev) CreateImage(context.Context, string, string) (*Image, error) {
	return nil, fmt.Errorf("not supported by the Docker dev provider")
}
func (d *DockerDev) DeleteImage(context.Context, string) error { return nil }
