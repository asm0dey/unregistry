package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/jsonmessage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/regclient/regclient"
	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestRegistryPushPull(t *testing.T) {
	ctx := context.Background()

	// Start unregistry in a Docker-in-Docker container with Docker using containerd image store.
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:    filepath.Join("..", ".."),
				Dockerfile: "Dockerfile.test",
				BuildOptionsModifier: func(buildOptions *types.ImageBuildOptions) {
					buildOptions.Target = "unregistry-dind"
				},
			},
			Env: map[string]string{
				"UNREGISTRY_LOG_LEVEL": "debug",
			},
			Privileged: true,
			// Explicitly specify the host port for the registry because if not specified, 'docker push' from Docker
			// Desktop is unable to reach the automatically mapped one for some reason.
			ExposedPorts: []string{"2375", "50000:5000"},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("2375"),
				wait.ForListeningPort("5000"),
			).WithStartupTimeoutDefault(15 * time.Second),
		},
		Started: true,
	}
	unregistryContainer, err := testcontainers.GenericContainer(ctx, req)
	require.NoError(t, err)

	t.Cleanup(func() {
		// Print last 20 lines of unregistry container logs.
		logs, err := unregistryContainer.Logs(ctx)
		assert.NoError(t, err, "Failed to get logs from unregistry container.")
		if err == nil {
			defer logs.Close()
			logsContent, err := io.ReadAll(logs)
			assert.NoError(t, err, "Failed to read logs from unregistry container.")
			if err == nil {

				lines := strings.Split(string(logsContent), "\n")
				start := len(lines) - 20
				if start < 0 {
					start = 0
				}

				t.Log("=== Last 20 lines of unregistry container logs ===")
				for i := start; i < len(lines); i++ {
					if lines[i] != "" {
						t.Log(lines[i])
					}
				}
				t.Log("=== End of unregistry container logs ===")
			}
		}

		// Ensure the container is terminated after the test.
		assert.NoError(t, unregistryContainer.Terminate(ctx))
	})

	mappedDockerPort, err := unregistryContainer.MappedPort(ctx, "2375")
	require.NoError(t, err)
	mappedRegistryPort, err := unregistryContainer.MappedPort(ctx, "5000")
	require.NoError(t, err)

	remoteCli, err := client.NewClientWithOpts(
		client.WithHost("tcp://localhost:"+mappedDockerPort.Port()),
		client.WithAPIVersionNegotiation(),
	)
	require.NoError(t, err)
	defer remoteCli.Close()

	registryAddr := "localhost:" + mappedRegistryPort.Port()
	t.Logf("Unregistry started at %s", registryAddr)

	localCli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer localCli.Close()

	// Check if local Docker uses containerd image store: https://docs.docker.com/engine/storage/containerd/
	info, err := localCli.Info(ctx)
	require.NoError(t, err)
	localDockerUsesContainerdImageStore := strings.Contains(
		fmt.Sprintf("%s", info.DriverStatus), "containerd.snapshotter",
	)

	t.Run("docker push/pull single-platform image", func(t *testing.T) {
		t.Parallel()

		imageName := "busybox:1.37.0-musl"
		registryImage := fmt.Sprintf("%s/%s", registryAddr, imageName)
		platform := "linux/amd64"
		ociPlatform := ocispec.Platform{Architecture: "amd64", OS: "linux"}
		indexDigest := "sha256:597bf7e5e8faf26b8efc4cb558eea5dc14d9cc97d5b4c8cdbe6404a7432d5a67"
		platformDigest := "sha256:008f65c96291274170bec5cf01b2de06dc049dc9d8f9bfb633520497875ed2c1"
		// Local image digest for the platform when *not* using containerd image store.
		dockerLocalDigest := "sha256:7da29d4d35b82e4412a41afd99398c64cc94d58fb5a701c73c684ed22201a14b"
		// Manifest digest created by 'docker push' when *not* using containerd image store.
		dockerDistribDigest := "sha256:f6e9a69f79d3bb745090d8bcd1d17ed24c1993d013d7b5b536fb7d0b61018ad7"

		t.Cleanup(
			func() {
				for _, img := range []string{imageName, registryImage} {
					_, err := localCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
					if !client.IsErrNotFound(err) {
						assert.NoError(t, err)
					}
					_, err = remoteCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
					if !client.IsErrNotFound(err) {
						assert.NoError(t, err)
					}
				}
			},
		)

		require.NoError(
			t, pullImage(ctx, localCli, imageName, image.PullOptions{Platform: platform}),
			"Failed to pull image '%s' locally", imageName,
		)
		img, _, err := localCli.ImageInspectWithRaw(ctx, imageName)
		require.NoError(t, err, "Failed to inspect image '%s' locally", imageName)
		if localDockerUsesContainerdImageStore {
			require.Equal(t, indexDigest, img.ID, "Image ID should match OCI index digest")
		} else {
			require.Equal(t, dockerLocalDigest, img.ID, "Image ID should match local Docker image digest")
		}

		// Tag and push the image to unregistry.
		require.NoError(
			t, localCli.ImageTag(ctx, imageName, registryImage), "Failed to tag image '%s' as '%s' locally",
			imageName,
			registryImage,
		)
		output, err := pushImage(ctx, localCli, registryImage, image.PushOptions{Platform: &ociPlatform})
		require.NoError(t, err, "Failed to push image '%s' to unregistry", registryImage)
		assert.NotContains(t, output, "Layer already exists")

		img, _, err = remoteCli.ImageInspectWithRaw(ctx, imageName)
		require.NoError(t, err, "Pushed image should appear in the remote Docker")
		if localDockerUsesContainerdImageStore {
			assert.Equal(t, platformDigest, img.ID, "Image ID should match platform-specific image digest")
		} else {
			assert.Equal(t, dockerDistribDigest, img.ID, "Image ID should match Docker distribution digest")
		}

		// Push the same image to test that it doesn't push the same layer again.
		output, err = pushImage(ctx, localCli, registryImage, image.PushOptions{Platform: &ociPlatform})
		require.NoError(t, err, "Failed to push image '%s' to unregistry", registryImage)
		assert.Contains(t, output, "Layer already exists", "Image should not be pushed again if it already exists")

		// Remove the image locally before pulling it back.
		for _, img := range []string{imageName, registryImage} {
			_, err = localCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
			require.NoError(t, err, "Failed to remove image '%s' locally", img)
		}

		// Pull the image back from unregistry.
		require.NoError(
			t, pullImage(ctx, localCli, registryImage, image.PullOptions{Platform: platform}),
			"Failed to pull image '%s' from unregistry", registryImage,
		)
		img, _, err = localCli.ImageInspectWithRaw(ctx, registryImage)
		require.NoError(t, err)
		if localDockerUsesContainerdImageStore {
			assert.Equal(t, platformDigest, img.ID, "Pulled image ID should match platform-specific image digest")
		} else {
			assert.Equal(t, dockerLocalDigest, img.ID, "Pulled image ID should match local Docker image digest")
		}

		// Remove the image locally again to test pulling it with arbitrary platform.
		_, err = localCli.ImageRemove(ctx, registryImage, image.RemoveOptions{PruneChildren: true})
		require.NoError(t, err, "Failed to remove image '%s' locally", img)

		// This is a bit weird, but it's the default behavior of the distribution registry.
		require.NoError(
			t, pullImage(ctx, localCli, registryImage, image.PullOptions{Platform: "linux/any-platform"}),
			"Pulling arbitrary platform should pull the existing platform-specific image",
		)

		img, _, err = localCli.ImageInspectWithRaw(ctx, registryImage)
		require.NoError(t, err)
		if localDockerUsesContainerdImageStore {
			assert.Equal(
				t, platformDigest, img.ID, "Arbitrary platform pull should match platform-specific image digest",
			)
		} else {
			assert.Equal(
				t, dockerLocalDigest, img.ID, "Arbitrary platform pull should match local Docker image digest",
			)
		}

		// Remove the image from remote Docker and try to pull it again.
		_, err = remoteCli.ImageRemove(ctx, imageName, image.RemoveOptions{PruneChildren: true})
		require.NoError(t, err, "Failed to remove image '%s' from remote Docker", imageName)

		require.ErrorContains(
			t, pullImage(ctx, localCli, registryImage, image.PullOptions{Platform: platform}),
			"not found",
			"Pulling image '%s' should fail after removing it from remote Docker", registryImage,
		)
	})

	t.Run("docker push multi-platform image (local containerd store)", func(t *testing.T) {
		if !localDockerUsesContainerdImageStore {
			t.Skip(
				"Skipping multi-platform image test that requires local Docker to use containerd image store.",
			)
		}
		t.Parallel()

		imageName := "traefik/whoami:v1.10.0"
		registryImage := fmt.Sprintf("%s/%s", registryAddr, imageName)
		platforms := []string{"linux/amd64", "linux/arm64", "linux/arm/v7"}

		t.Cleanup(
			func() {
				for _, img := range []string{imageName, registryImage} {
					_, err := localCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
					if !client.IsErrNotFound(err) {
						assert.NoError(t, err)
					}
					_, err = remoteCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
					if !client.IsErrNotFound(err) {
						assert.NoError(t, err)
					}
				}
			},
		)

		// Pull the image locally for all platforms.
		for _, platform := range platforms {
			require.NoError(
				t, pullImage(ctx, localCli, imageName, image.PullOptions{Platform: platform}),
				"Failed to pull image '%s' locally for platform '%s'", imageName, platform,
			)
		}

		summary, err := localCli.ImageList(ctx, image.ListOptions{
			Filters: filters.NewArgs(
				filters.Arg("reference", imageName),
			),
			Manifests: true,
		})
		require.NoError(t, err, "Failed to list image '%s' locally", imageName)
		manifests := summary[0].Manifests
		require.Len(
			t, manifests, len(platforms), "Image '%s' should have %d manifests", imageName, len(platforms),
		)
		manifestDigests := make([]string, len(manifests))
		for i, m := range manifests {
			require.True(t, m.Available, "Manifest '%s' should be available", m.ID)
			manifestDigests[i] = m.ID
		}

		// Tag and push the multi-platform image to unregistry.
		require.NoError(
			t, localCli.ImageTag(ctx, imageName, registryImage),
			"Failed to tag image '%s' as '%s' locally", imageName, registryImage,
		)
		output, err := pushImage(ctx, localCli, registryImage, image.PushOptions{}) // all platforms
		require.NoError(t, err, "Failed to push multi-platform image '%s' to unregistry", registryImage)
		assert.Contains(t, output, "Pushed", "Layers should be pushed to unregistry")
		assert.NotContains(t, output, "Layer already exists")

		// Check the image in remote Docker is the same as in local Docker.
		remoteSummary, err := remoteCli.ImageList(ctx, image.ListOptions{
			Filters: filters.NewArgs(
				filters.Arg("reference", imageName),
			),
			Manifests: true,
		})
		require.NoError(t, err, "Failed to list image '%s' in remote Docker", imageName)
		require.Len(t, remoteSummary, 1, "Image '%s' should be available in remote Docker", imageName)
		assert.Equal(t, summary[0].ID, remoteSummary[0].ID, "Image ID should match after pushing to unregistry")

		remoteManifests := remoteSummary[0].Manifests
		require.Len(
			t, remoteManifests, len(platforms), "Remote image '%s' should have %d manifests", imageName,
			len(platforms),
		)
		remoteManifestDigests := make([]string, len(remoteManifests))
		for i, m := range remoteManifests {
			require.True(t, m.Available, "Remote manifest '%s' should be available", m.ID)
			remoteManifestDigests[i] = m.ID
		}
		assert.ElementsMatch(
			t, manifestDigests, remoteManifestDigests,
			"Manifest digests should match after pushing to unregistry",
		)

		// Push the same image to test that it doesn't push the same layer again.
		output, err = pushImage(ctx, localCli, registryImage, image.PushOptions{})
		require.NoError(t, err, "Failed to push multi-platform image '%s' to unregistry", registryImage)
		assert.Contains(
			t, output, "Layer already exists", "Layers should not be pushed again if they already exists",
		)
		assert.NotContains(t, output, "Pushed", "No new layers should be pushed")
	})

	t.Run("docker pull from partially available multi-platform image", func(t *testing.T) {
		t.Parallel()

		imageName := "busybox:1.37.0-glibc"
		registryImage := fmt.Sprintf("%s/%s", registryAddr, imageName)
		indexDigest := "sha256:210ce53959959e79523b8cb0f0bb1cf1c49bf9747cdedb47db1cf0db8e642f61"
		amd64Digest := "sha256:7c0ffe5751238c8479f952f3fbc3b719d47bccac0e9bf0a21c77a27cba9ef12d"
		arm64Digest := "sha256:68a0d55a75c935e1101d16ded1c748babb7f96a9af43f7533ba83b87e2508b82"
		amd64DockerDigest := "sha256:6d3e4188a38af91b0c1577b9e88c53368926b2fe0e1fb985d6e8a70040520c4d"
		arm64DockerDigest := "sha256:7b4721e214600044496305a20ca3902677e572127d4d976ed0e54da0137c243a"

		// This image has multiple platforms, we'll pull only 2 of them in remote Docker.
		availablePlatforms := []string{"linux/amd64", "linux/arm64/v8"}
		missingPlatform := "linux/arm/v7"

		t.Cleanup(func() {
			_, err := localCli.ImageRemove(ctx, registryImage, image.RemoveOptions{PruneChildren: true})
			if !client.IsErrNotFound(err) {
				assert.NoError(t, err)
			}
			_, err = remoteCli.ImageRemove(ctx, imageName, image.RemoveOptions{PruneChildren: true})
			if !client.IsErrNotFound(err) {
				assert.NoError(t, err)
			}
		})

		// First, pull only the selected platforms to remote Docker.
		for _, platform := range availablePlatforms {
			require.NoError(
				t, pullImage(ctx, remoteCli, imageName, image.PullOptions{Platform: platform}),
				"Failed to pull image '%s' to remote Docker for platform '%s'", imageName, platform,
			)
		}

		// Test 1: Pull available platforms - should succeed.
		for _, platform := range availablePlatforms {
			err = pullImage(ctx, localCli, registryImage, image.PullOptions{Platform: platform})
			require.NoError(t, err, "Failed to pull available platform '%s' from unregistry", platform)

			// Verify the image was pulled successfully if not using containerd image store.
			if !localDockerUsesContainerdImageStore {
				img, _, err := localCli.ImageInspectWithRaw(ctx, registryImage)
				require.NoError(t, err, "Failed to inspect image '%s' pulled for platform '%s'",
					registryImage, platform)

				if platform == "linux/amd64" {
					assert.Equal(t, amd64DockerDigest, img.ID,
						"Image ID for platform '%s' should match digest", platform)
				} else if platform == "linux/arm64/v8" {
					assert.Equal(t, arm64DockerDigest, img.ID,
						"Image ID for platform '%s' should match digest", platform)
				}
			}
		}
		// Verify the image was pulled successfully if using containerd image store.
		if localDockerUsesContainerdImageStore {
			summary, err := localCli.ImageList(ctx, image.ListOptions{
				Filters: filters.NewArgs(
					filters.Arg("reference", registryImage),
				),
				Manifests: true,
			})
			require.NoError(t, err, "Failed to list image '%s' locally", registryImage)
			require.Len(t, summary, 1, "Image '%s' should be available locally after pulling", registryImage)
			assert.Equal(t, indexDigest, summary[0].ID, "Image ID should match OCI index digest")

			assert.True(t, slices.ContainsFunc(summary[0].Manifests, func(m image.ManifestSummary) bool {
				if m.ID == amd64Digest {
					assert.True(t, m.Available, "Image content for linux/amd64 should be available", amd64Digest)
					return true
				}
				return false
			}), "Image for linux/amd64 should be available")

			assert.True(t, slices.ContainsFunc(summary[0].Manifests, func(m image.ManifestSummary) bool {
				if m.ID == arm64Digest {
					assert.True(t, m.Available, "Image content for linux/arm64/v8 should be available", arm64Digest)
					return true
				}
				return false
			}), "Image for linux/arm64/v8 should be available")
		}

		// Test 2: Pull missing platform - should fail with "not found".
		err = pullImage(ctx, localCli, registryImage, image.PullOptions{Platform: missingPlatform})
		if localDockerUsesContainerdImageStore {
			// This is a weird behavior (bug?) of containerd image store. It returns "Image is up to date"
			// for missing platform.
			assert.NoError(t, err, "Pulling missing platform '%s' should succeed with Image is up to date")
		} else {
			assert.ErrorContains(t, err, "not found", "Pulling missing platform '%s' should fail with 'not found'")
		}
	})

	t.Run("docker push/pull image with external registry prefix", func(t *testing.T) {
		t.Parallel()

		imageName := "ghcr.io/containerd/busybox:1.36"
		registryImage := fmt.Sprintf("%s/%s", registryAddr, imageName)

		t.Cleanup(
			func() {
				for _, img := range []string{imageName, registryImage} {
					_, err := localCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
					if !client.IsErrNotFound(err) {
						assert.NoError(t, err)
					}
				}
				_, err = remoteCli.ImageRemove(ctx, imageName, image.RemoveOptions{PruneChildren: true})
				if !client.IsErrNotFound(err) {
					assert.NoError(t, err)
				}
			},
		)

		require.NoError(t, pullImage(ctx, localCli, imageName, image.PullOptions{}),
			"Failed to pull image '%s' locally", imageName)

		// Tag the image with external registry prefix and push it to unregistry.
		require.NoError(t, localCli.ImageTag(ctx, imageName, registryImage),
			"Failed to tag image '%s' as '%s' locally", imageName, registryImage)
		_, err := pushImage(ctx, localCli, registryImage, image.PushOptions{})
		require.NoError(t, err, "Failed to push image '%s' to unregistry", registryImage)

		// Verify the image appears in remote Docker with the external registry prefix.
		_, _, err = remoteCli.ImageInspectWithRaw(ctx, imageName)
		require.NoError(t, err, "Pushed image should appear in remote Docker with external registry prefix")

		// Remove the image locally before pulling it back.
		for _, img := range []string{imageName, registryImage} {
			_, err = localCli.ImageRemove(ctx, img, image.RemoveOptions{PruneChildren: true})
			require.NoError(t, err, "Failed to remove image '%s' locally", img)
		}

		// Pull the image back from unregistry using the full path with external prefix.
		require.NoError(t, pullImage(ctx, localCli, registryImage, image.PullOptions{}),
			"Failed to pull image '%s' from unregistry", registryImage)
	})

	tarballImageTests := []struct {
		name            string
		tarPath         string
		image           string
		digest          string
		manifestDigests []string
	}{
		{
			name:            "push/pull single-platform OCI image with regclient",
			tarPath:         filepath.Join("images", "busybox:1.36.1-musl-amd64_oci.tar"),
			image:           "busybox:1.36.1-musl-amd64",
			digest:          "sha256:e56bc0f7fc7d4452b17eb4ac0a9261ff4c9a469afa45d2b673e03650716d095d",
			manifestDigests: []string{"sha256:e56bc0f7fc7d4452b17eb4ac0a9261ff4c9a469afa45d2b673e03650716d095d"},
		},
		{
			name:            "push/pull single-platform Docker image with regclient",
			tarPath:         filepath.Join("images", "busybox:1.36.0-uclibc-arm64.tar"),
			image:           "busybox:1.36.0-uclibc-arm64",
			digest:          "sha256:32e7b5cc125a7c45c6ba9e7924b11123b2c5e880b8965c3075d53140d4fd14bf",
			manifestDigests: []string{"sha256:32e7b5cc125a7c45c6ba9e7924b11123b2c5e880b8965c3075d53140d4fd14bf"},
		},
		{
			name:    "push/pull multi-platform OCI image with regclient",
			tarPath: filepath.Join("images", "busybox:1.37.0-uclibc_multi_oci.tar"),
			image:   "busybox:1.37.0-uclibc",
			digest:  "sha256:cc57e0ff4b6d3138931ff5c7180d18078813300e2508a25fb767a4d36df30d4d",
			manifestDigests: []string{
				"sha256:06c4f3f3ef84198a9d3f3a308258dd7a0b69b36d808ad2c9173c003968df90cf",
				"sha256:46300c35aa557736b6fffd302205498b90b71afc0fd6ce4587cd8be0dda7b1b3",
				"sha256:a2ec48a1cbd23376b4f8d54b37f0732df6fd9b428996a24bf48c149a0c130fc7",
				"sha256:ef6a3791fc5987128fbe4fc9bc4dcc585ef87274b1224e008c1ec7e8d73ecd65",
				"sha256:4182ed714d722cbfdc5f42a5e1f9e138361984d543cc3a27c2cb06cc17f6b8fd",
				"sha256:57b98e4b2e1700a64a9efb4e22f4a1321f8da41a8762dda3d4c0bf38f3403e31",
			},
		},
		{
			name:    "push/pull multi-platform Docker image with regclient",
			tarPath: filepath.Join("images", "busybox:1.36.0-musl_multi.tar"),
			image:   "busybox:1.36.0-musl",
			digest:  "sha256:07f673568590f568182a127cb6a804dd5822101e3fc6b16a40a2868315be236c",
			manifestDigests: []string{
				"sha256:77357250c9fbf1dc4e74992163ef45642dfada33076639178f46bee6fb34c3d9",
				"sha256:946858083a14c5bcdb0287d8b29e59fd064567c0454e1f6b7e5f988f7b84e6a1",
				"sha256:03ca03375cef94b1c09f38b51d1b88e7db30855492e6ee4e3818e0219ed27292",
				"sha256:e9f2feba92e3f5ed3de67e928b72126ad4a9d97d3712e89d0dc1bff49aca98fa",
				"sha256:51240564e556984b9f7af480dfafaea2561721b6adfafbdc7352b98804cd8c4f",
				"sha256:947136b8ef9e77417e7b57d7819bbcf23a6037de99c8293dfcbe561826c78ba1",
				"sha256:b6b03614034b26bb175ed7716d9f20bbebb51dfb9707e1178edc7d734f51236b",
			},
		},
	}

	for _, tt := range tarballImageTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registryImage := fmt.Sprintf("%s/%s", registryAddr, tt.image)

			t.Cleanup(func() {
				_, err = remoteCli.ImageRemove(ctx, tt.image, image.RemoveOptions{PruneChildren: true})
				if !client.IsErrNotFound(err) {
					assert.NoError(t, err)
				}
			})

			// Push the OCI tarball image to unregistry using regclient.
			rc, err := newRegClient(registryImage)
			require.NoError(t, err, "Failed to create regclient for registry image '%s'", registryImage)
			defer rc.Close(ctx)

			err = rc.pushTarballImage(ctx, tt.tarPath)
			require.NoError(t, err, "Failed to push tarball image to unregistry")

			// Verify the image is available in remote Docker including all platform images.
			remoteSummary, err := remoteCli.ImageList(
				ctx, image.ListOptions{
					Filters: filters.NewArgs(
						filters.Arg("reference", tt.image),
					),
					Manifests: true,
				},
			)
			require.NoError(t, err, "Failed to list image in remote Docker")
			require.Len(t, remoteSummary, 1, "Image should be available in remote Docker")

			// Get manifest details and verify platforms.
			manifests := remoteSummary[0].Manifests
			var remoteManifestDigests []string
			for _, m := range manifests {
				require.True(t, m.Available, "Manifest %s should be available", m.ID)
				// Skip attestation and unknown manifests.
				if m.Kind == image.ManifestKindImage {
					remoteManifestDigests = append(remoteManifestDigests, m.ID)
				}
			}
			assert.ElementsMatch(t, tt.manifestDigests, remoteManifestDigests,
				"Manifest digests should match after pushing to unregistry")

			// Verify the pushed image can be pulled using regclient.
			m, err := rc.ManifestGet(ctx, rc.Ref)
			require.NoError(t, err, "Failed to get manifest for '%s' from unregistry", tt.image)
			require.Equal(t, tt.digest, m.GetDescriptor().Digest.String(),
				"Manifest digests should match after pushing to unregistry")

			err = rc.ImageExport(ctx, rc.Ref, io.Discard)
			require.NoError(t, err, "Failed to pull image '%s' from unregistry", tt.image)
		})
	}
}

func pullImage(ctx context.Context, cli *client.Client, imageName string, opts image.PullOptions) error {
	respBody, err := cli.ImagePull(ctx, imageName, opts)
	if err != nil {
		return err
	}
	defer respBody.Close()

	decoder := json.NewDecoder(respBody)
	errCh := make(chan error, 1)

	go func() {
		var jm jsonmessage.JSONMessage
		for {
			if err = decoder.Decode(&jm); err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
					return
				}
				errCh <- fmt.Errorf("decode image pull message: %v", err)
				return
			}

			if jm.Error != nil {
				errCh <- fmt.Errorf("pull failed for '%s': %s", imageName, jm.Error.Message)
				return
			}
		}
	}()

	for {
		select {
		case err = <-errCh:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func pushImage(ctx context.Context, cli *client.Client, imageName string, opts image.PushOptions) (string, error) {
	if opts.RegistryAuth == "" {
		opts.RegistryAuth = base64.URLEncoding.EncodeToString([]byte("{}"))
	}

	respBody, err := cli.ImagePush(ctx, imageName, opts)
	if err != nil {
		return "", err
	}
	defer respBody.Close()

	decoder := json.NewDecoder(respBody)
	errCh := make(chan error, 1)

	var output []string
	go func() {
		var jm jsonmessage.JSONMessage
		for {
			if err = decoder.Decode(&jm); err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
					return
				}
				errCh <- fmt.Errorf("decode image push message: %v", err)
				return
			}

			if jm.Error != nil {
				errCh <- fmt.Errorf("push failed for '%s': %s", imageName, jm.Error.Message)
				return
			}

			if jm.ID != "" {
				output = append(output, fmt.Sprintf("%s: %s", jm.ID, jm.Status))
			} else {
				output = append(output, jm.Status)
			}
		}
	}()

	for {
		select {
		case err = <-errCh:
			return strings.Join(output, "\n"), err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// regClient is a wrapper around regclient.RegClient to work with a specific repository reference.
type regClient struct {
	*regclient.RegClient
	Ref ref.Ref
}

func newRegClient(repo string) (*regClient, error) {
	host, _, _ := strings.Cut(repo, "/")
	rc := regclient.New(regclient.WithConfigHost(config.Host{
		Name: host,
		TLS:  config.TLSDisabled,
	}))

	r, err := ref.New(repo)
	if err != nil {
		return nil, fmt.Errorf("parse repository reference: %v", err)
	}

	return &regClient{
		RegClient: rc,
		Ref:       r,
	}, nil
}

func (rc *regClient) Close(ctx context.Context) error {
	return rc.RegClient.Close(ctx, rc.Ref)
}

// pushTarballImage pushes an image from OCI tarball to the registry.
func (rc *regClient) pushTarballImage(ctx context.Context, tarPath string) error {
	tarReader, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("open tarball file '%s': %v", tarPath, err)
	}
	defer tarReader.Close()

	if err = rc.ImageImport(ctx, rc.Ref, tarReader); err != nil {
		return fmt.Errorf("import image from tarball '%s': %v", tarPath, err)
	}

	return nil
}
