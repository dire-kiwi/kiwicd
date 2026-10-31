package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/nektos/act/pkg/container"
)

// What act creates for a job (containers, their volumes, the images of
// Docker actions) is named with the job's prefix, and so is the network an
// isolated job runs on. Names look like kiwicd-1a2b3c4d-j1234-CI-build-<hash>,
// with the registration's agent ID: runner and job IDs come from the
// server's database, so agents of different servers that share a Docker
// daemon have the same ones.
func jobPrefix(agentID string, jobID int64) string {
	return fmt.Sprintf("%s%d-", agentPrefix(agentID), jobID)
}

func agentPrefix(agentID string) string {
	return fmt.Sprintf("kiwicd-%s-j", agentID)
}

// oldAgentPrefix is how agents named their jobs' Docker objects before
// registrations had agent IDs.
func oldAgentPrefix(runnerID int64) string {
	return fmt.Sprintf("kiwicd-r%d-j", runnerID)
}

// bridgeName is the network interface of a job's network: bridgePrefix, so
// a firewall can match the agent's jobs (as in iptables -i kiwicd-+), and 8
// base-36 digits of a hash of the network's name, which is the job's alone.
// Linux allows 15 characters.
func bridgeName(bridgePrefix, network string) string {
	h := sha256.Sum256([]byte(network))
	const digits = 2821109907456 // 36^8
	s := strconv.FormatUint(binary.BigEndian.Uint64(h[:8])%digits, 36)
	return bridgePrefix + strings.Repeat("0", 8-len(s)) + s
}

// createJobNetwork creates the bridge network an isolated job's containers
// share. Each job has its own, so jobs can't reach each other's containers.
func createJobNetwork(ctx context.Context, name, bridge string) error {
	cli, err := container.GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.NetworkCreate(ctx, name, dockerclient.NetworkCreateOptions{
		Driver:  "bridge",
		Options: map[string]string{"com.docker.network.bridge.name": bridge},
		Labels:  map[string]string{"kiwicd.job": name},
	})
	return err
}

// pingDocker checks that the agent can reach the Docker daemon.
func pingDocker(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cli, err := container.GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	_, err = cli.Ping(ctx, dockerclient.PingOptions{})
	return err
}

// removeDockerObjects removes the containers, networks, volumes and images
// whose names start with prefix: what act left of a job (it normally removes
// its containers and volumes itself, but not when the job is stopped
// outright or the agent dies) and the job's network.
func removeDockerObjects(ctx context.Context, prefix string) error {
	cli, err := container.GetDockerClient(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	var errs []error
	containers, err := cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: dockerclient.Filters{}.Add("name", prefix)})
	errs = append(errs, err)
	for _, c := range containers.Items {
		if hasPrefixedName(c.Names, prefix) {
			_, err := cli.ContainerRemove(ctx, c.ID, dockerclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			errs = append(errs, err)
		}
	}
	networks, err := cli.NetworkList(ctx, dockerclient.NetworkListOptions{Filters: dockerclient.Filters{}.Add("name", prefix)})
	errs = append(errs, err)
	for _, n := range networks.Items {
		if strings.HasPrefix(n.Name, prefix) {
			_, err := cli.NetworkRemove(ctx, n.ID, dockerclient.NetworkRemoveOptions{})
			errs = append(errs, err)
		}
	}
	volumes, err := cli.VolumeList(ctx, dockerclient.VolumeListOptions{Filters: dockerclient.Filters{}.Add("name", prefix)})
	errs = append(errs, err)
	for _, v := range volumes.Items {
		if strings.HasPrefix(v.Name, prefix) {
			_, err := cli.VolumeRemove(ctx, v.Name, dockerclient.VolumeRemoveOptions{Force: true})
			errs = append(errs, err)
		}
	}
	images, err := cli.ImageList(ctx, dockerclient.ImageListOptions{Filters: dockerclient.Filters{}.Add("reference", prefix+"*")})
	errs = append(errs, err)
	for _, img := range images.Items {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, prefix) {
				_, err := cli.ImageRemove(ctx, tag, dockerclient.ImageRemoveOptions{Force: true})
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// hasPrefixedName reports whether a container is named with prefix. The
// API's name filter matches anywhere in the name, and names start with "/".
func hasPrefixedName(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(strings.TrimPrefix(n, "/"), prefix) {
			return true
		}
	}
	return false
}
