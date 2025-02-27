/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/compose-spec/compose-go/types"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/buildx/build"
	_ "github.com/docker/buildx/driver/docker" // required to get default driver registered
	"github.com/docker/buildx/util/buildflags"
	xprogress "github.com/docker/buildx/util/progress"
	"github.com/docker/cli/cli/command"
	"github.com/docker/docker/pkg/urlutil"
	bclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	specs "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"
)

func (s *composeService) Build(ctx context.Context, project *types.Project, options api.BuildOptions) error {
	return progress.Run(ctx, func(ctx context.Context) error {
		return s.build(ctx, project, options)
	})
}

func (s *composeService) build(ctx context.Context, project *types.Project, options api.BuildOptions) error {
	opts := map[string]build.Options{}
	imagesToBuild := []string{}

	args := flatten(options.Args.Resolve(func(s string) (string, bool) {
		s, ok := project.Environment[s]
		return s, ok
	}))

	services, err := project.GetServices(options.Services...)
	if err != nil {
		return err
	}

	for _, service := range services {
		if service.Build != nil {
			imageName := getImageName(service, project.Name)
			imagesToBuild = append(imagesToBuild, imageName)
			buildOptions, err := s.toBuildOptions(project, service, imageName)
			if err != nil {
				return err
			}
			buildOptions.Pull = options.Pull
			buildOptions.BuildArgs = mergeArgs(buildOptions.BuildArgs, args)
			buildOptions.NoCache = options.NoCache
			buildOptions.CacheFrom, err = buildflags.ParseCacheEntry(service.Build.CacheFrom)
			if err != nil {
				return err
			}

			for _, image := range service.Build.CacheFrom {
				buildOptions.CacheFrom = append(buildOptions.CacheFrom, bclient.CacheOptionsEntry{
					Type:  "registry",
					Attrs: map[string]string{"ref": image},
				})
			}

			opts[imageName] = buildOptions
		}
	}

	_, err = s.doBuild(ctx, project, opts, options.Progress)
	if err == nil {
		if len(imagesToBuild) > 0 && !options.Quiet {
			utils.DisplayScanSuggestMsg()
		}
	}

	return err
}

func (s *composeService) ensureImagesExists(ctx context.Context, project *types.Project, quietPull bool) error {
	for _, service := range project.Services {
		if service.Image == "" && service.Build == nil {
			return fmt.Errorf("invalid service %q. Must specify either image or build", service.Name)
		}
	}

	images, err := s.getLocalImagesDigests(ctx, project)
	if err != nil {
		return err
	}

	err = s.pullRequiredImages(ctx, project, images, quietPull)
	if err != nil {
		return err
	}

	mode := xprogress.PrinterModeAuto
	if quietPull {
		mode = xprogress.PrinterModeQuiet
	}
	opts, err := s.getBuildOptions(project, images)
	if err != nil {
		return err
	}
	builtImages, err := s.doBuild(ctx, project, opts, mode)
	if err != nil {
		return err
	}

	if len(builtImages) > 0 {
		utils.DisplayScanSuggestMsg()
	}
	for name, digest := range builtImages {
		images[name] = digest
	}
	// set digest as com.docker.compose.image label so we can detect outdated containers
	for i, service := range project.Services {
		image := getImageName(service, project.Name)
		digest, ok := images[image]
		if ok {
			if project.Services[i].Labels == nil {
				project.Services[i].Labels = types.Labels{}
			}
			project.Services[i].Labels[api.ImageDigestLabel] = digest
			project.Services[i].Image = image
		}
	}
	return nil
}

func (s *composeService) getBuildOptions(project *types.Project, images map[string]string) (map[string]build.Options, error) {
	opts := map[string]build.Options{}
	for _, service := range project.Services {
		if service.Image == "" && service.Build == nil {
			return nil, fmt.Errorf("invalid service %q. Must specify either image or build", service.Name)
		}
		imageName := getImageName(service, project.Name)
		_, localImagePresent := images[imageName]

		if service.Build != nil {
			if localImagePresent && service.PullPolicy != types.PullPolicyBuild {
				continue
			}
			opt, err := s.toBuildOptions(project, service, imageName)
			if err != nil {
				return nil, err
			}
			opts[imageName] = opt
			continue
		}
	}
	return opts, nil

}

func (s *composeService) getLocalImagesDigests(ctx context.Context, project *types.Project) (map[string]string, error) {
	imageNames := []string{}
	for _, s := range project.Services {
		imgName := getImageName(s, project.Name)
		if !utils.StringContains(imageNames, imgName) {
			imageNames = append(imageNames, imgName)
		}
	}
	imgs, err := s.getImages(ctx, imageNames)
	if err != nil {
		return nil, err
	}
	images := map[string]string{}
	for name, info := range imgs {
		images[name] = info.ID
	}
	return images, nil
}

func (s *composeService) serverInfo(ctx context.Context) (command.ServerInfo, error) {
	ping, err := s.apiClient.Ping(ctx)
	if err != nil {
		return command.ServerInfo{}, err
	}
	serverInfo := command.ServerInfo{
		HasExperimental: ping.Experimental,
		OSType:          ping.OSType,
		BuildkitVersion: ping.BuilderVersion,
	}
	return serverInfo, err
}

func (s *composeService) doBuild(ctx context.Context, project *types.Project, opts map[string]build.Options, mode string) (map[string]string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	serverInfo, err := s.serverInfo(ctx)
	if err != nil {
		return nil, err
	}
	if buildkitEnabled, err := command.BuildKitEnabled(serverInfo); err != nil || !buildkitEnabled {
		return s.doBuildClassic(ctx, opts)
	}
	return s.doBuildBuildkit(ctx, project, opts, mode)
}

func (s *composeService) toBuildOptions(project *types.Project, service types.ServiceConfig, imageTag string) (build.Options, error) {
	var tags []string
	tags = append(tags, imageTag)

	buildArgs := flatten(service.Build.Args.Resolve(func(s string) (string, bool) {
		s, ok := project.Environment[s]
		return s, ok
	}))

	var plats []specs.Platform
	if platform, ok := project.Environment["DOCKER_DEFAULT_PLATFORM"]; ok {
		p, err := platforms.Parse(platform)
		if err != nil {
			return build.Options{}, err
		}
		plats = append(plats, p)
	}
	if service.Platform != "" {
		p, err := platforms.Parse(service.Platform)
		if err != nil {
			return build.Options{}, err
		}
		plats = append(plats, p)
	}

	return build.Options{
		Inputs: build.Inputs{
			ContextPath:    service.Build.Context,
			DockerfilePath: dockerFilePath(service.Build.Context, service.Build.Dockerfile),
		},
		BuildArgs:   buildArgs,
		Tags:        tags,
		Target:      service.Build.Target,
		Exports:     []bclient.ExportEntry{{Type: "image", Attrs: map[string]string{}}},
		Platforms:   plats,
		Labels:      service.Build.Labels,
		NetworkMode: service.Build.Network,
		ExtraHosts:  service.Build.ExtraHosts,
		Session: []session.Attachable{
			authprovider.NewDockerAuthProvider(os.Stderr),
		},
	}, nil
}

func flatten(in types.MappingWithEquals) types.Mapping {
	if len(in) == 0 {
		return nil
	}
	out := types.Mapping{}
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = *v
	}
	return out
}

func mergeArgs(m ...types.Mapping) types.Mapping {
	merged := types.Mapping{}
	for _, mapping := range m {
		for key, val := range mapping {
			merged[key] = val
		}
	}
	return merged
}

func dockerFilePath(context string, dockerfile string) string {
	if urlutil.IsGitURL(context) || filepath.IsAbs(dockerfile) {
		return dockerfile
	}
	return filepath.Join(context, dockerfile)
}
