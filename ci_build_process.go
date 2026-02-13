package npminstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paketo-buildpacks/libnodejs"
	"github.com/paketo-buildpacks/packit/v2/chronos"
	"github.com/paketo-buildpacks/packit/v2/fs"
	"github.com/paketo-buildpacks/packit/v2/pexec"
	"github.com/paketo-buildpacks/packit/v2/scribe"
)

type CIBuildProcess struct {
	executable  Executable
	summer      Summer
	environment EnvironmentConfig
	clock       chronos.Clock
	logger      scribe.Logger
}

func NewCIBuildProcess(executable Executable, summer Summer, environment EnvironmentConfig, clock chronos.Clock, logger scribe.Logger) CIBuildProcess {
	return CIBuildProcess{
		executable:  executable,
		summer:      summer,
		environment: environment,
		clock:       clock,
		logger:      logger,
	}
}

func (r CIBuildProcess) ShouldRun(workingDir string, metadata map[string]interface{}, npmrcConfig string) (bool, string, error) {
	cachedNodeVersion, err := cacheExecutableResponse(
		r.executable,
		[]string{"get", "user-agent"},
		workingDir,
		npmrcConfig,
		r.logger)
	if err != nil {
		return false, "", fmt.Errorf("failed to execute npm get user-agent: %w", err)
	}
	defer func() {
		if removeErr := os.Remove(cachedNodeVersion); removeErr != nil {
			r.logger.Subprocess("Warning: failed to remove temporary file %s: %s", cachedNodeVersion, removeErr)
		}
	}()

	sum, err := r.summer.Sum(
		filepath.Join(workingDir, "package.json"),
		filepath.Join(workingDir, "package-lock.json"),
		cachedNodeVersion)
	if err != nil {
		return false, "", err
	}

	runPostInstall, postInstallScripts := r.shouldRunPostInstall()

	// Ensures cache is invalidated if post-install scripts change
	if runPostInstall {
		newHash := sha256.New()
		newHash.Write([]byte(sum))
		newHash.Write([]byte(postInstallScripts))
		sum = hex.EncodeToString(newHash.Sum(nil))
	}

	cacheSha, ok := metadata["cache_sha"].(string)
	if !ok || sum != cacheSha {
		return true, sum, nil
	}

	return false, "", nil
}

func (r CIBuildProcess) Run(modulesDir, cacheDir, workingDir, npmrcPath string, launch bool) error {
	err := os.MkdirAll(filepath.Join(workingDir, "node_modules"), os.ModePerm)
	if err != nil {
		return err
	}

	environment := os.Environ()

	if value, ok := r.environment.Lookup("NPM_CONFIG_LOGLEVEL"); ok {
		environment = append(environment, fmt.Sprintf("NPM_CONFIG_LOGLEVEL=%s", value))
	}

	if npmrcPath != "" {
		environment = append(environment, fmt.Sprintf("NPM_CONFIG_GLOBALCONFIG=%s", npmrcPath))
	}

	if !launch {
		environment = append(environment, "NODE_ENV=development")
	}

	args := []string{"ci", "--unsafe-perm", "--cache", cacheDir}
	r.logger.Subprocess("Running 'npm %s'", strings.Join(args, " "))

	err = r.executable.Execute(pexec.Execution{
		Args:   args,
		Dir:    workingDir,
		Stdout: r.logger.ActionWriter,
		Stderr: r.logger.ActionWriter,
		Env:    environment,
	})
	if err != nil {
		return fmt.Errorf("npm ci failed: %w", err)
	}

	runPostInstall, postInstallScripts := r.shouldRunPostInstall()
	if runPostInstall {
		err := r.runPostInstallScripts(workingDir, postInstallScripts, environment)
		if err != nil {
			return fmt.Errorf("failed to run post-install scripts: %w", err)
		}
	}

	_, err = os.Stat(filepath.Join(workingDir, "node_modules"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("unable to stat node_modules in working directory: %w", err)
	}

	err = fs.Move(filepath.Join(workingDir, "node_modules"), filepath.Join(modulesDir, "node_modules"))
	if err != nil {
		return err
	}

	err = os.Symlink(filepath.Join(modulesDir, "node_modules"), filepath.Join(workingDir, "node_modules"))
	if err != nil {
		return err
	}

	return nil
}

func (r CIBuildProcess) shouldRunPostInstall() (bool, string) {
	scripts, ok := r.environment.Lookup(PostInstallScripts)
	if !ok || scripts == "" {
		return false, ""
	}
	return true, scripts
}

func (r CIBuildProcess) runPostInstallScripts(workingDir string, postInstallScripts string, environment []string) error {
	scriptsToRun, err := scriptsToRun(workingDir, postInstallScripts)
	if err != nil {
		return err
	}

	duration, err := r.clock.Measure(func() error {
		for _, script := range scriptsToRun {
			r.logger.Subprocess("Running 'npm run %s'", script)

			err := r.executable.Execute(pexec.Execution{
				Args:   []string{"run", script},
				Dir:    workingDir,
				Stdout: r.logger.ActionWriter,
				Stderr: r.logger.ActionWriter,
				Env:    environment,
			})
			if err != nil {
				return err
			}

			r.logger.Break()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to run post-install scripts: %w", err)
	}

	r.logger.Detail("Completed in %s", duration.Round(time.Millisecond))
	r.logger.Break()

	return nil
}

func scriptsToRun(workingDir string, postInstallScripts string) ([]string, error) {
	scripts := strings.Split(postInstallScripts, ",")
	for i := range scripts {
		scripts[i] = strings.TrimSpace(scripts[i])
	}

	packageJSON, err := libnodejs.ParsePackageJSON(workingDir)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, script := range scripts {
		if _, ok := packageJSON.AllScripts[script]; !ok {
			missing = append(missing, script)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("could not find script(s) %s in package.json", missing)
	}

	return scripts, nil
}
