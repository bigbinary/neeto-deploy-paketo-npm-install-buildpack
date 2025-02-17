package npminstall

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/paketo-buildpacks/libnodejs"
	"github.com/paketo-buildpacks/packit/v2/pexec"
	"github.com/paketo-buildpacks/packit/v2/sbom"

	"github.com/paketo-buildpacks/packit/v2"
	"github.com/paketo-buildpacks/packit/v2/chronos"
	"github.com/paketo-buildpacks/packit/v2/fs"
	"github.com/paketo-buildpacks/packit/v2/scribe"
)

const NODE_MODULES_CACHE = "node_modules_cache"

//go:generate faux --interface BuildManager --output fakes/build_manager.go
type BuildManager interface {
	Resolve(workingDir string) (BuildProcess, bool, error)
}

//go:generate faux --interface EntryResolver --output fakes/entry_resolver.go
type EntryResolver interface {
	MergeLayerTypes(string, []packit.BuildpackPlanEntry) (launch, build bool)
}

//go:generate faux --interface SBOMGenerator --output fakes/sbom_generator.go
type SBOMGenerator interface {
	Generate(dir string) (sbom.SBOM, error)
}

//go:generate faux --interface ConfigurationManager --output fakes/configuration_manager.go
type ConfigurationManager interface {
	DeterminePath(typ, platformDir, entry string) (path string, err error)
}

//go:generate faux --interface PruneProcess --output fakes/prune_process.go
type PruneProcess interface {
	ShouldRun(workingDir string, metadata map[string]interface{}, npmrcPath string) (run bool, sha string, err error)
	Run(modulesDir, cacheDir, workingDir, npmrcPath string, launch bool) error
}

//go:generate faux --interface Symlinker --output fakes/symlinker.go
type Symlinker interface {
	WithPath(path string) Symlinker
	Link(source, target string) error
}

//go:generate faux --interface SymlinkResolver --output fakes/symlink_resolver.go
type SymlinkResolver interface {
	ParseLockfile(lockfilePath string) (Lockfile, error)
	Copy(lockfilePath, sourceLayerPath, targetLayerPath string) error
	Resolve(lockfilePath, layerPath string) error
}

func Build(entryResolver EntryResolver,
	configurationManager ConfigurationManager,
	buildManager BuildManager,
	pruneProcess PruneProcess,
	clock chronos.Clock,
	logger scribe.Emitter,
	sbomGenerator SBOMGenerator,
	linker Symlinker,
	environment EnvironmentConfig,
	symlinkResolver SymlinkResolver,
) packit.BuildFunc {
	return func(context packit.BuildContext) (packit.BuildResult, error) {
		logger.Title("%s %s", context.BuildpackInfo.Name, context.BuildpackInfo.Version)

		globalNpmrcPath, err := configurationManager.DeterminePath("npmrc", context.Platform.Path, ".npmrc")
		if err != nil {
			return packit.BuildResult{}, err
		}

		logger.Process("Resolving installation process")

		projectPath, err := libnodejs.FindProjectPath(context.WorkingDir)
		if err != nil {
			return packit.BuildResult{}, err
		}

		npmVersion, found := environment.Lookup("BP_NPM_VERSION")
		if found {
			logger.Process("Installling custom npm version %s", npmVersion)
			args := []string{"install", fmt.Sprintf("npm@%s", npmVersion)}
			logger.Subprocess("Running 'npm %s'", strings.Join(args, " "))

			err = pexec.NewExecutable("npm").Execute(pexec.Execution{
				Args:   args,
				Dir:    projectPath,
				Stdout: logger.ActionWriter,
				Stderr: logger.ActionWriter,
			})
			if err != nil {
				return packit.BuildResult{}, fmt.Errorf("update of npm failed: %w", err)
			}
			moduleBinPath := filepath.Join(projectPath, "node_modules", ".bin")
			localBinPath := filepath.Join(projectPath, "node_modules", ".bin_local")
			err = os.Mkdir(localBinPath, os.ModePerm)
			if err != nil {
				return packit.BuildResult{}, err
			}
			err = os.Link(path.Join(moduleBinPath, "npm"), filepath.Join(localBinPath, "npm"))
			if err != nil {
				return packit.BuildResult{}, err
			}
			os.Setenv("PATH", fmt.Sprintf("%s:%s:%s", filepath.Join(localBinPath), os.Getenv("PATH"), moduleBinPath))
		}

		npmCacheLayer, err := context.Layers.Get(LayerNameCache)
		if err != nil {
			return packit.BuildResult{}, err
		}

		npmCacheLayer.Cache = true

		process, cacheFound, err := buildManager.Resolve(projectPath)
		if err != nil {
			return packit.BuildResult{}, err
		}

		if cacheFound {
			npmCacheLayer, err = UpdateNpmCacheLayer(logger, projectPath, npmCacheLayer)
			if err != nil {
				return packit.BuildResult{}, err
			}
		}

		sbomDisabled, err := environment.LookupBool("BP_DISABLE_SBOM")
		if err != nil {
			return packit.BuildResult{}, err
		}

		launch, build := entryResolver.MergeLayerTypes(NodeModules, context.Plan.Entries)

		var layers []packit.Layer
		// var buildLayerPath string
		if build {
			layer, err := context.Layers.Get("build-modules")
			if err != nil {
				return packit.BuildResult{}, err
			}
			// buildLayerPath = layer.Path

			run, sha, err := process.ShouldRun(projectPath, layer.Metadata, globalNpmrcPath)
			if err != nil {
				return packit.BuildResult{}, err
			}

			if run {
				logger.Process("Executing build environment install process")

				layer, err = layer.Reset()
				if err != nil {
					return packit.BuildResult{}, err
				}

				duration, err := clock.Measure(func() error {
					return process.Run(layer.Path, npmCacheLayer.Path, projectPath, globalNpmrcPath, false)
				})
				if err != nil {
					return packit.BuildResult{}, err
				}

				// err = linker.Link(filepath.Join(projectPath, "node_modules"), filepath.Join(layer.Path, "node_modules"))
				// if err != nil {
				// 	return packit.BuildResult{}, err
				// }

				// err = symlinkResolver.Resolve(filepath.Join(projectPath, "package-lock.json"), layer.Path)
				// if err != nil {
				// 	return packit.BuildResult{}, err
				// }

				logger.Action("Completed in %s", duration.Round(time.Millisecond))
				logger.Break()

				layer.Metadata = map[string]interface{}{
					"cache_sha": sha,
				}

				if globalNpmrcPath != "" {
					layer.BuildEnv.Default("NPM_CONFIG_GLOBALCONFIG", globalNpmrcPath)
				}
				nodeModulesPath := filepath.Join(projectPath, "node_modules")
				layer.BuildEnv.Append("PATH", filepath.Join(nodeModulesPath, ".bin"), string(os.PathListSeparator))
				layer.BuildEnv.Prepend("PATH", filepath.Join(nodeModulesPath, ".bin_local"), string(os.PathListSeparator))
				layer.BuildEnv.Override("NODE_ENV", "development")

				logger.EnvironmentVariables(layer)

				if sbomDisabled {
					logger.Subprocess("Skipping SBOM generation for Node Install")
					logger.Break()
				} else {
					logger.GeneratingSBOM(layer.Path)

					var sbomContent sbom.SBOM
					duration, err = clock.Measure(func() error {
						sbomContent, err = sbomGenerator.Generate(context.WorkingDir)
						return err
					})
					if err != nil {
						return packit.BuildResult{}, err
					}
					logger.Action("Completed in %s", duration.Round(time.Millisecond))
					logger.Break()

					logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)

					layer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
					if err != nil {
						return packit.BuildResult{}, err
					}
				}
			} else {
				logger.Process("Reusing cached layer %s", layer.Path)

				err = fs.Copy(filepath.Join(layer.Path, "node_modules"), filepath.Join(projectPath, "node_modules"))
				if err != nil {
					fmt.Println(err)
				}
				fmt.Println("Copy successful.")

			}
			layer.Build = true
			layer.Cache = true

			layers = append(layers, layer)
		}

		if launch {
			layer, err := context.Layers.Get("launch-modules")
			if err != nil {
				return packit.BuildResult{}, err
			}

			run, sha, err := process.ShouldRun(projectPath, layer.Metadata, globalNpmrcPath)
			if err != nil {
				return packit.BuildResult{}, err
			}

			if run {
				logger.Process("Executing launch environment install process")
				fmt.Println("SHA:",sha)

				layer, err = layer.Reset()
				if err != nil {
					return packit.BuildResult{}, err
				}

				// if build {
				// 	err := fs.Copy(filepath.Join(buildLayerPath, "node_modules"), filepath.Join(projectPath, "node_modules"))
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// 	process = pruneProcess
				// }

				// duration, err := clock.Measure(func() error {
				// 	return process.Run(layer.Path, npmCacheLayer.Path, projectPath, globalNpmrcPath, true)
				// })
				// if err != nil {
				// 	return packit.BuildResult{}, err
				// }
				// targetLayerPath := layer.Path

				// if build {
				// 	err = fs.Move(filepath.Join(projectPath, "node_modules"), filepath.Join(layer.Path, "node_modules"))
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}

				// 	targetLayerPath = buildLayerPath
				// }

				// layer.ExecD = []string{filepath.Join(context.CNBPath, "bin", "setup-symlinks")}

				// err = linker.Link(filepath.Join(projectPath, "node_modules"), filepath.Join(targetLayerPath, "node_modules"))
				// if err != nil {
				// 	return packit.BuildResult{}, err
				// }

				// keepBuildCache, _ := environment.Lookup("BP_KEEP_NODE_BUILD_CACHE")
				// if keepBuildCache != "true" {
				// 	linkName := filepath.Join(layer.Path, "node_modules", ".cache")
				// 	err = os.RemoveAll(linkName)
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}

				// 	cacheFolder := filepath.Join(os.TempDir(), NODE_MODULES_CACHE)
				// 	err = os.Symlink(cacheFolder, linkName)
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// }

				// if build {
				// 	err = symlinkResolver.Copy(filepath.Join(projectPath, "package-lock.json"), buildLayerPath, layer.Path)
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// } else {
				// 	err = symlinkResolver.Resolve(filepath.Join(projectPath, "package-lock.json"), targetLayerPath)
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// }

				// logger.Action("Completed in %s", duration.Round(time.Millisecond))
				// logger.Break()

				// layer.Metadata = map[string]interface{}{
				// 	"cache_sha": sha,
				// }

				layer.LaunchEnv.Default("NPM_CONFIG_LOGLEVEL", "error")
				layer.LaunchEnv.Default("NODE_PROJECT_PATH", projectPath)
				nodeModulesPath := filepath.Join(projectPath, "node_modules")
				layer.LaunchEnv.Append("PATH", filepath.Join(nodeModulesPath, ".bin"), string(os.PathListSeparator))
				layer.LaunchEnv.Prepend("PATH", filepath.Join(nodeModulesPath, ".bin_local"), string(os.PathListSeparator))

				logger.EnvironmentVariables(layer)

				// if sbomDisabled {
				// 	logger.Subprocess("Skipping SBOM generation for Node Install")
				// 	logger.Break()
				// } else {
				// 	logger.GeneratingSBOM(layer.Path)

				// 	var sbomContent sbom.SBOM
				// 	duration, err = clock.Measure(func() error {
				// 		sbomContent, err = sbomGenerator.Generate(context.WorkingDir)
				// 		return err
				// 	})
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// 	logger.Action("Completed in %s", duration.Round(time.Millisecond))
				// 	logger.Break()

				// 	logger.FormattingSBOM(context.BuildpackInfo.SBOMFormats...)

				// 	layer.SBOM, err = sbomContent.InFormats(context.BuildpackInfo.SBOMFormats...)
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// }
			} else {
				logger.Process("Reusing cached layer %s", layer.Path)
				// if !build {
				// 	err = linker.Link(filepath.Join(projectPath, "node_modules"), filepath.Join(layer.Path, "node_modules"))
				// 	if err != nil {
				// 		return packit.BuildResult{}, err
				// 	}
				// }
			}

			layer.Launch = true

			layers = append(layers, layer)
		}

		exists, err := fs.Exists(npmCacheLayer.Path)
		if exists {
			if !fs.IsEmptyDir(npmCacheLayer.Path) {
				layers = append(layers, npmCacheLayer)
			}
		}
		if err != nil {
			return packit.BuildResult{}, err
		}

		logger.Break()

		return packit.BuildResult{Layers: layers}, nil
	}
}
