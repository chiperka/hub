package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"chiperka-hub/internal/cloud"
)

// cloudProviders builds the providers that have credentials in the environment.
func cloudProviders() map[string]cloud.Provider {
	providers := map[string]cloud.Provider{}
	if tok := os.Getenv("CHIPERKA_CLOUD_HETZNER_TOKEN"); tok != "" {
		providers["hetzner"] = cloud.NewHetzner(tok)
	}
	if key := os.Getenv("CHIPERKA_CLOUD_AWS_ACCESS_KEY_ID"); key != "" {
		providers["aws"] = cloud.NewAWS(env("CHIPERKA_CLOUD_AWS_REGION", "eu-central-1"), key, os.Getenv("CHIPERKA_CLOUD_AWS_SECRET_ACCESS_KEY"))
	}
	// Development only: "servers" are local worker containers, see cloud.DockerDev.
	if as := os.Getenv("CHIPERKA_CLOUD_DEV_DOCKER"); as == "hetzner" || as == "aws" {
		providers[as] = &cloud.DockerDev{As: as, HubURL: env("CHIPERKA_CLOUD_DEV_HUB_URL", "http://host.docker.internal:8080")}
	}
	return providers
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// runCloudCommand handles `chiperka-hub cloud <janitor|build-image>`, used by
// scheduled CI pipelines. Credentials come from the same variables as the hub's.
func runCloudCommand(args []string) int {
	usage := "usage: chiperka-hub cloud janitor [--max-age 5h] [--dry-run]\n" +
		"       chiperka-hub cloud build-image --provider hetzner|aws [--type T] [--location L] [--image IMG ...]"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	providers := cloudProviders()
	if len(providers) == 0 {
		fmt.Fprintln(os.Stderr, "no provider credentials: set CHIPERKA_CLOUD_HETZNER_TOKEN and/or CHIPERKA_CLOUD_AWS_ACCESS_KEY_ID + CHIPERKA_CLOUD_AWS_SECRET_ACCESS_KEY")
		return 2
	}

	switch args[0] {
	case "janitor":
		fs := flag.NewFlagSet("janitor", flag.ExitOnError)
		maxAge := fs.Duration("max-age", cloud.SelfDestructAfter, "delete Chiperka servers older than this")
		dry := fs.Bool("dry-run", false, "only print what would be deleted")
		fs.Parse(args[1:])
		if err := cloud.Janitor(ctx, providers, *maxAge, *dry, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "janitor:", err)
			return 1
		}
		return 0

	case "build-image":
		fs := flag.NewFlagSet("build-image", flag.ExitOnError)
		prov := fs.String("provider", "", "hetzner or aws")
		typ := fs.String("type", "", "builder server type (default cpx22 / c7a.large)")
		loc := fs.String("location", "", "builder location (default nbg1 / eu-central-1a)")
		worker := fs.String("worker-image", env("CHIPERKA_WORKER_IMAGE", "finie/chiperka-worker:latest"), "worker image to bake in")
		keep := fs.Int("keep", 2, "images to keep")
		disk := fs.Int("disk-gb", cloud.DefaultImageDiskGB, "AWS: builder disk, the minimum disk of servers started from the image")
		cidr := fs.String("ssh-cidr", "", "AWS: create/update the security group first, allowing SSH from this range")
		var images listFlag
		fs.Var(&images, "image", "extra image to pre-pull (repeatable), e.g. test images from master")
		fs.Parse(args[1:])
		p := providers[*prov]
		if p == nil {
			fmt.Fprintf(os.Stderr, "provider %q is not configured\n", *prov)
			return 2
		}
		o := cloud.BuildImageOptions{
			Type: *typ, Location: *loc, WorkerImage: *worker, Images: images, Keep: *keep, DiskGB: *disk, SSHAllowCIDR: *cidr,
			Registry: os.Getenv("CHIPERKA_REGISTRY_HOST"), RegistryUser: os.Getenv("CHIPERKA_REGISTRY_USER"),
			RegistryPassword: os.Getenv("CHIPERKA_REGISTRY_PASSWORD"),
		}
		if o.Type == "" {
			o.Type = map[string]string{"hetzner": "cpx22", "aws": "c7a.large"}[*prov]
		}
		if o.Location == "" {
			o.Location = map[string]string{"hetzner": "nbg1", "aws": "eu-central-1a"}[*prov]
		}
		if _, err := cloud.BuildImage(ctx, p, o, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "build-image:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}
