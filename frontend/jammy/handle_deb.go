package jammy

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/dalec"
	"github.com/Azure/dalec/frontend"
	"github.com/Azure/dalec/frontend/deb"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const JammyWorkerContextName = "dalec-jammy-worker"

func handleDeb(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
	return frontend.BuildWithPlatform(ctx, client, func(ctx context.Context, client gwclient.Client, platform *ocispecs.Platform, spec *dalec.Spec, targetKey string) (gwclient.Reference, *dalec.DockerImageSpec, error) {
		sOpt, err := frontend.SourceOptFromClient(ctx, client)
		if err != nil {
			return nil, nil, err
		}

		st, err := buildDeb(ctx, client, spec, sOpt, targetKey, dalec.ProgressGroup("Building Jammy deb package: "+spec.Name))
		if err != nil {
			return nil, nil, err
		}

		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, nil, err
		}

		res, err := client.Solve(ctx, gwclient.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, nil, err
		}

		ref, err := res.SingleRef()
		if err != nil {
			return nil, nil, err
		}
		if platform == nil {
			p := platforms.DefaultSpec()
			platform = &p
		}
		return ref, &dalec.DockerImageSpec{Image: ocispecs.Image{Platform: *platform}}, nil
	})
}

func installPackages(ls ...string) llb.RunOption {
	return dalec.RunOptFunc(func(ei *llb.ExecInfo) {
		// This only runs apt-get update if the pkgcache is older than 10 minutes.
		dalec.ShArgs(`set -ex; apt update; apt install -y ` + strings.Join(ls, " ")).SetRunOption(ei)
		dalec.WithMountedAptCache(AptCachePrefix).SetRunOption(ei)
	})
}

func installWithConstraints(pkgPath string, pkgName string) llb.RunOption {
	return dalec.RunOptFunc(func(ei *llb.ExecInfo) {
		// The apt solver always tries to select the latest package version even when constraints specify that an older version should be installed and that older version is available in a repo.
		// This leads the solver to simply refuse to install our target package if the latest version of ANY dependency package is incompatible with the constraints.
		// To work around this we first install the .deb for the package with dpkg, specifically ignoring any dependencies so that we can avoid the constraints issue.
		// We then use aptitude to fix the (possibly broken) install of the package, and we pass the aptitude solver a hint to REJECT any solution that involves uninstalling the package.
		// This forces aptitude to find a solution that will respect the constraints even if the solution involves pinning dependency packages to older versions.
		dalec.ShArgs(`set -ex; dpkg -i --force-depends ` + pkgPath +
			fmt.Sprintf(`; apt update; aptitude install -y -f -o "Aptitude::ProblemResolver::Hints::=reject %s :UNINST"`, pkgName)).SetRunOption(ei)
		dalec.WithMountedAptCache(AptCachePrefix).SetRunOption(ei)
	})
}

func buildDeb(ctx context.Context, client gwclient.Client, spec *dalec.Spec, sOpt dalec.SourceOpts, targetKey string, opts ...llb.ConstraintsOpt) (llb.State, error) {
	worker, err := workerBase(sOpt, opts...)
	if err != nil {
		return llb.Scratch(), err
	}

	versionID, err := deb.ReadDistroVersionID(ctx, client, worker)
	if err != nil {
		return llb.Scratch(), err
	}

	installBuildDeps, err := buildDepends(worker, sOpt, spec, targetKey, opts...)
	if err != nil {
		return llb.Scratch(), errors.Wrap(err, "error creating deb for build dependencies")
	}

	worker = worker.With(installBuildDeps)
	st, err := deb.BuildDeb(worker, spec, sOpt, targetKey, versionID, opts...)
	if err != nil {
		return llb.Scratch(), err
	}

	signed, err := frontend.MaybeSign(ctx, client, st, spec, targetKey, sOpt)
	if err != nil {
		return llb.Scratch(), err
	}
	return signed, nil
}

func workerBase(sOpt dalec.SourceOpts, opts ...llb.ConstraintsOpt) (llb.State, error) {
	base, err := sOpt.GetContext(jammyRef, dalec.WithConstraints(opts...))
	if err != nil {
		return llb.Scratch(), err
	}
	if base != nil {
		return *base, nil
	}

	base, err = sOpt.GetContext(JammyWorkerContextName, dalec.WithConstraints(opts...))
	if err != nil {
		return llb.Scratch(), err
	}

	if base != nil {
		return *base, nil
	}

	return llb.Image(jammyRef, llb.WithMetaResolver(sOpt.Resolver)).With(basePackages(opts...)), nil
}

func basePackages(opts ...llb.ConstraintsOpt) llb.StateOption {
	return func(in llb.State) llb.State {
		opts = append(opts, dalec.ProgressGroup("Install base packages"))
		return in.Run(
			installPackages("aptitude", "dpkg-dev", "devscripts", "equivs", "fakeroot", "dh-make", "build-essential", "dh-apparmor", "dh-make", "dh-exec", "debhelper-compat="+deb.DebHelperCompat),
			dalec.WithConstraints(opts...),
		).Root()
	}
}

func buildDepends(worker llb.State, sOpt dalec.SourceOpts, spec *dalec.Spec, targetKey string, opts ...llb.ConstraintsOpt) (llb.StateOption, error) {
	deps := spec.Dependencies
	if t, ok := spec.Targets[targetKey]; ok {
		if t.Dependencies != nil {
			deps = t.Dependencies
		}
	}

	var buildDeps map[string]dalec.PackageConstraints
	if deps != nil {
		buildDeps = deps.Build
	}

	if len(buildDeps) == 0 {
		return func(in llb.State) llb.State {
			return in
		}, nil
	}

	depsSpec := &dalec.Spec{
		Name:     spec.Name + "-deps",
		Packager: "Dalec",
		Version:  spec.Version,
		Revision: spec.Revision,
		Dependencies: &dalec.PackageDependencies{
			Runtime: buildDeps,
		},
		Description: "Build dependencies for " + spec.Name,
	}

	pg := dalec.ProgressGroup("Install build dependencies")
	opts = append(opts, pg)
	pkg, err := deb.BuildDeb(worker, depsSpec, sOpt, targetKey, "", append(opts, dalec.ProgressGroup("Create intermediate deb for build dependnencies"))...)
	if err != nil {
		return nil, errors.Wrap(err, "error creating intermediate package for installing build dependencies")
	}

	return func(in llb.State) llb.State {
		const (
			debPath = "/tmp/dalec/internal/build/deps"
		)

		return in.Run(
			installWithConstraints(debPath+"/*.deb", depsSpec.Name),
			llb.AddMount(debPath, pkg, llb.Readonly),
			dalec.WithConstraints(opts...),
		).Root()
	}, nil
}
