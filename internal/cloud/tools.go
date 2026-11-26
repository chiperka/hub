package cloud

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"
)

// Janitor deletes Chiperka servers older than maxAge at every provider. It runs
// as a scheduled CI job independently of the hub, so servers get cleaned up even
// when the hub is down. It ignores hub IDs on purpose.
func Janitor(ctx context.Context, providers map[string]Provider, maxAge time.Duration, dryRun bool, out io.Writer) error {
	var firstErr error
	for name, p := range providers {
		insts, err := p.List(ctx)
		if err != nil {
			fmt.Fprintf(out, "%s: list failed: %v\n", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		kept := 0
		for _, in := range insts {
			if in.State == StateTerminated {
				continue
			}
			age := time.Since(in.CreatedAt)
			if age < maxAge {
				kept++
				continue
			}
			if dryRun {
				fmt.Fprintf(out, "%s: would delete %s (%s, %s old)\n", name, in.Name, in.ID, age.Round(time.Minute))
				continue
			}
			if err := p.Delete(ctx, in.ID); err != nil {
				fmt.Fprintf(out, "%s: delete %s failed: %v\n", name, in.Name, err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			fmt.Fprintf(out, "%s: deleted %s (%s, %s old)\n", name, in.Name, in.ID, age.Round(time.Minute))
		}
		fmt.Fprintf(out, "%s: %d younger server(s) left alone\n", name, kept)
	}
	return firstErr
}

type BuildImageOptions struct {
	Type, Location   string
	WorkerImage      string
	Images           []string // test and helper images to pre-pull
	Registry         string
	RegistryUser     string
	RegistryPassword string
	Keep             int           // how many images to keep, newest first
	SSHAllowCIDR     string        // AWS only: create/update the security group first
	Timeout          time.Duration // for provisioning
}

// BuildImage boots a builder server from stock Ubuntu, lets cloud-init install
// Docker and pre-pull images, waits for it to power off, snapshots it and
// removes old images. New worker servers start from the newest image.
func BuildImage(ctx context.Context, p Provider, o BuildImageOptions, out io.Writer) (*Image, error) {
	if o.Keep < 1 {
		o.Keep = 2
	}
	if o.Timeout <= 0 {
		o.Timeout = 40 * time.Minute
	}
	name := "chiperka-worker-" + time.Now().UTC().Format("20060102-1504")
	labels := map[string]string{LabelManaged: "true", LabelHub: "builder"}

	// A throwaway key only so Hetzner does not e-mail a root password; nobody logs in.
	_, pub, _, err := newKey("chiperka-builder")
	if err != nil {
		return nil, err
	}
	if h, ok := p.(*Hetzner); ok {
		defer h.DeleteSSHKey(context.Background(), "chiperka-hub-builder")
	}
	// The builder has its own firewall / security group: nobody needs SSH to it.
	cidr := o.SSHAllowCIDR
	if cidr == "" {
		cidr = "127.0.0.1/32"
	}
	if err := p.Prepare(ctx, PrepareRequest{HubID: "builder", SSHAllowCIDR: cidr, HubPublicKey: pub}); err != nil {
		return nil, err
	}
	base, err := p.BaseImage(ctx)
	if err != nil {
		return nil, err
	}
	ud, err := ImageUserData{
		WorkerImage: o.WorkerImage, Images: o.Images,
		Registry: o.Registry, RegistryUser: o.RegistryUser, RegistryPassword: o.RegistryPassword,
	}.Render()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "creating builder %s (%s in %s) from %s\n", name, o.Type, o.Location, base)
	inst, err := p.Create(ctx, CreateRequest{
		Name: name, Type: o.Type, Location: o.Location, Image: base, UserData: ud, DiskGB: 60, Labels: labels, StopOnShutdown: true,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := p.Delete(context.Background(), inst.ID); err != nil {
			fmt.Fprintf(out, "warning: could not delete builder %s: %v (the janitor will)\n", inst.ID, err)
		} else {
			fmt.Fprintf(out, "builder %s deleted\n", inst.ID)
		}
	}()

	fmt.Fprintf(out, "waiting for provisioning to finish (the builder powers off)\n")
	wctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	start := time.Now()
	for {
		insts, err := p.List(wctx)
		if err == nil {
			for _, in := range insts {
				if in.ID != inst.ID {
					continue
				}
				if in.State == StateTerminated {
					return nil, fmt.Errorf("builder %s was terminated before it could be snapshotted", inst.ID)
				}
				if in.State == StateStopped {
					goto stopped
				}
			}
		}
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("provisioning did not finish within %s", o.Timeout)
		case <-time.After(10 * time.Second):
		}
	}
stopped:
	fmt.Fprintf(out, "provisioned in %s, creating image %s\n", time.Since(start).Round(time.Second), name)
	img, err := p.CreateImage(ctx, inst.ID, name)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "image %s ready (%s)\n", img.ID, name)

	imgs, err := p.ListImages(ctx)
	if err == nil {
		sort.Slice(imgs, func(i, j int) bool { return imgs[i].CreatedAt.After(imgs[j].CreatedAt) })
		for i, old := range imgs {
			if i < o.Keep || old.ID == img.ID {
				continue
			}
			if err := p.DeleteImage(ctx, old.ID); err != nil {
				fmt.Fprintf(out, "warning: could not delete old image %s: %v\n", old.ID, err)
			} else {
				fmt.Fprintf(out, "deleted old image %s (%s)\n", old.ID, old.Name)
			}
		}
	}
	return img, nil
}
