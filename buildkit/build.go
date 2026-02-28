package buildkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
	_ "github.com/moby/buildkit/client/connhelper/nerdctlcontainer"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/appcontext"
	_ "github.com/moby/buildkit/util/grpcutil/encoding/proto"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/railwayapp/railpack/core/plan"
	"github.com/tonistiigi/fsutil"
)

const (
	buildkitHostNotSetError = `BUILDKIT_HOST environment variable is not set.

To start a local BuildKit daemon and set the environment variable run:

	docker run --rm --privileged -d --name buildkit moby/buildkit
	export BUILDKIT_HOST='docker-container://buildkit'`

	buildkitInfoError = `failed to get buildkit information.

Most likely the $BUILDKIT_HOST is not running. Here's an example of how to start the build container:

	docker run --rm --privileged -d --name buildkit moby/buildkit

Use 'railpack --verbose' to view more error details.
		`
)

type BuildWithBuildkitClientOptions struct {
	ImageName    string
	DumpLLB      bool
	OutputDir    string
	ProgressMode string
	SecretsHash  string
	Secrets      map[string]string
	Platform     string
	ImportCache  string
	ExportCache  string
	CacheKey     string
	GitHubToken  string
}

func BuildWithBuildkitClient(appDir string, plan *plan.BuildPlan, opts BuildWithBuildkitClientOptions) error {
	ctx := appcontext.Context()

	imageName := opts.ImageName
	if imageName == "" {
		imageName = getImageName(appDir)
	}

	buildkitHost := os.Getenv("BUILDKIT_HOST")
	if buildkitHost == "" {
		return errors.New(buildkitHostNotSetError)
	}

	log.Debugf("Connecting to buildkit host: %s", buildkitHost)

	// connecting to the buildkit host does *not* mean the specified build container is running
	c, err := client.New(ctx, buildkitHost)
	if err != nil {
		return fmt.Errorf("failed to connect to buildkit: %w", err)
	}
	defer c.Close()

	// Get the buildkit info early so we can ensure we can connect to the buildkit host
	info, err := c.Info(ctx)
	if err != nil {
		log.Debugf("error getting buildkit info: %v", err)
		return errors.New(buildkitInfoError)
	}

	// Parse the platform string using our helper function
	buildPlatform, err := ParsePlatformWithDefaults(opts.Platform)
	if err != nil {
		return fmt.Errorf("failed to parse platform '%s': %w", opts.Platform, err)
	}

	llbState, image, err := ConvertPlanToLLB(plan, ConvertPlanOptions{
		BuildPlatform: buildPlatform,
		SecretsHash:   opts.SecretsHash,
		CacheKey:      opts.CacheKey,
		GitHubToken:   opts.GitHubToken,
	})
	if err != nil {
		return fmt.Errorf("error converting plan to LLB: %w", err)
	}

	imageBytes, err := json.Marshal(image)
	if err != nil {
		return fmt.Errorf("error marshalling image: %w", err)
	}

	def, err := llbState.Marshal(ctx, llb.LinuxAmd64)
	if err != nil {
		return fmt.Errorf("error marshaling LLB state: %w", err)
	}

	if opts.DumpLLB {
		err = llb.WriteTo(def, os.Stdout)
		if err != nil {
			return fmt.Errorf("error writing LLB definition: %w", err)
		}
		return nil
	}

	ch := make(chan *client.SolveStatus)

	var imageTempFile *os.File
	if opts.OutputDir == "" {
		var err error
		imageTempFile, err = os.CreateTemp("", "railpack-image-*.tar")
		if err != nil {
			return fmt.Errorf("failed to create temp file for image: %w", err)
		}
		defer func() {
			imageTempFile.Close()
			os.Remove(imageTempFile.Name())
		}()
	}

	progressDone := make(chan bool)
	go func() {
		displayCh := make(chan *client.SolveStatus)
		go func() {
			for s := range ch {
				displayCh <- s
			}
			close(displayCh)
		}()

		progressMode := progressui.AutoMode
		if opts.ProgressMode == "plain" {
			progressMode = progressui.PlainMode
		} else if opts.ProgressMode == "tty" {
			progressMode = progressui.TtyMode
		}

		display, err := progressui.NewDisplay(os.Stdout, progressMode)
		if err != nil {
			log.Error("failed to create progress display", "error", err)
		}

		_, err = display.UpdateFrom(ctx, displayCh)
		if err != nil {
			log.Error("failed to update progress display", "error", err)
		}
		progressDone <- true
	}()

	appFS, err := fsutil.NewFS(appDir)
	if err != nil {
		return fmt.Errorf("error creating FS: %w", err)
	}

	log.Debugf("Building image for %s with BuildKit %s", platforms.Format(buildPlatform), info.BuildkitVersion.Version)

	secretsMap := make(map[string][]byte)
	for k, v := range opts.Secrets {
		secretsMap[k] = []byte(v)
	}
	secrets := secretsprovider.FromMap(secretsMap)

	solveOpts := client.SolveOpt{
		LocalMounts: map[string]fsutil.FS{
			"context": appFS,
		},
		Session: []session.Attachable{secrets},
		Exports: []client.ExportEntry{
			{
				Type: client.ExporterDocker,
				Attrs: map[string]string{
					"name":                  imageName,
					"containerimage.config": string(imageBytes),
				},
				Output: func(_ map[string]string) (io.WriteCloser, error) {
					return imageTempFile, nil
				},
			},
		},
	}

	// Add cache import if specified
	if opts.ImportCache != "" {
		solveOpts.CacheImports = append(solveOpts.CacheImports, client.CacheOptionsEntry{
			Type:  "gha",
			Attrs: parseKeyValue(opts.ImportCache),
		})
	}

	// Add cache export if specified
	if opts.ExportCache != "" {
		solveOpts.CacheExports = append(solveOpts.CacheExports, client.CacheOptionsEntry{
			Type:  "gha",
			Attrs: parseKeyValue(opts.ExportCache),
		})
	}

	// Save the resulting filesystem to a directory
	if opts.OutputDir != "" {
		err = os.MkdirAll(opts.OutputDir, 0755)
		if err != nil {
			return fmt.Errorf("error creating output directory: %w", err)
		}

		solveOpts = client.SolveOpt{
			LocalMounts: map[string]fsutil.FS{
				"context": appFS,
			},
			Exports: []client.ExportEntry{
				{
					Type:      client.ExporterLocal,
					OutputDir: opts.OutputDir,
				},
			},
		}
	}

	startTime := time.Now()
	_, err = c.Solve(ctx, def, solveOpts, ch)

	// Wait for progress monitoring to complete
	<-progressDone

	if err != nil {
		return fmt.Errorf("failed to solve: %w", err)
	}

	// Only load image if we didn't export to a directory
	if opts.OutputDir == "" {
		log.Infof("Export finished, loading image into docker...")

		// Reopen the file for reading since BuildKit closed the write handle
		f, err := os.Open(imageTempFile.Name())
		if err != nil {
			return fmt.Errorf("failed to open temp image file: %w", err)
		}
		defer f.Close()

		// Try to find the best container engine (prefer podman, then docker)
		engine := "docker"
		if p, err := exec.LookPath("podman"); err == nil {
			engine = p
		} else if d, err := exec.LookPath("docker"); err == nil {
			engine = d
		}

		args := []string{"load", "-i", f.Name()}
		// On Windows, wrap .cmd/.bat in cmd /c
		if runtime.GOOS == "windows" && (strings.HasSuffix(strings.ToLower(engine), ".cmd") || strings.HasSuffix(strings.ToLower(engine), ".bat")) {
			args = append([]string{"/c", engine}, args...)
			engine = "cmd"
		}

		log.Infof("Running: %s %s", engine, strings.Join(args, " "))
		cmd := exec.Command(engine, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("container engine load failed (%s): %w", engine, err)
		}
	}

	buildDuration := time.Since(startTime)
	log.Infof("Successfully built image in %.2fs", buildDuration.Seconds())

	if opts.OutputDir != "" {
		log.Infof("Saved image filesystem to directory `%s`", opts.OutputDir)
	} else {
		log.Infof("Run with `docker run -it %s`", imageName)
	}

	return nil
}

func getImageName(appDir string) string {
	if appDir == "" || strings.HasSuffix(appDir, string(filepath.Separator)) || strings.HasSuffix(appDir, "/") {
		return "railpack-app"
	}
	name := filepath.Base(appDir)
	if name == "" || name == "." {
		return "railpack-app"
	}
	// Docker requires image names to be lowercase
	return strings.ToLower(name)
}

// Helper function to parse key=value strings into a map
func parseKeyValue(s string) map[string]string {
	attrs := make(map[string]string)
	parts := strings.Split(s, ",")
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			attrs[kv[0]] = kv[1]
		}
	}
	return attrs
}
