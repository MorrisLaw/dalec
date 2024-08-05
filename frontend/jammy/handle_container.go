package jammy

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"

	"github.com/Azure/dalec"
	"github.com/Azure/dalec/frontend"
	"github.com/Azure/dalec/frontend/pkg/bkfs"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	jammyRef = "mcr.microsoft.com/mirror/docker/library/ubuntu:jammy"

	testRepoPath           = "/opt/testrepo"
	testRepoSourceListPath = "/etc/apt/sources.list.d/test-dalec-local-repo.list"
)

// Unlike how azlinux works, when creating a debian system there are many implicit
// dpeendencies which are already expected to be on the system that are not in
// the package dependency tree.
// These are the extra packages we'll include when building a container.
var (
	baseDeps = []string{
		"base-files",
		"base-passwd",
		"usrmerge",
	}
	systemdDeps = []string{
		"init-system-helpers",
		"bash",
		"systemctl",
		"dash",
	}
)

func handleContainer(ctx context.Context, client gwclient.Client) (*gwclient.Result, error) {
	return frontend.BuildWithPlatform(ctx, client, func(ctx context.Context, client gwclient.Client, platform *ocispecs.Platform, spec *dalec.Spec, targetKey string) (gwclient.Reference, *dalec.DockerImageSpec, error) {
		sOpt, err := frontend.SourceOptFromClient(ctx, client)
		if err != nil {
			return nil, nil, err
		}

		opt := dalec.ProgressGroup("Building Jammy container: " + spec.Name)

		deb, err := buildDeb(ctx, client, spec, sOpt, targetKey, opt)
		if err != nil {
			return nil, nil, err
		}

		worker, err := workerBase(sOpt, opt)
		if err != nil {
			return nil, nil, err
		}

		var includeTestRepo bool

		workerFS, err := bkfs.FromState(ctx, &worker, client)
		if err != nil {
			return nil, nil, err
		}

		// Check if there there is a test repo in the worker image.
		// We'll mount that into the target container while installing packages.
		_, repoErr := fs.Stat(workerFS, testRepoPath[1:])
		_, listErr := fs.Stat(workerFS, testRepoSourceListPath[1:])
		if listErr == nil && repoErr == nil {
			// This is a test and we need to include the repo from the worker image
			// into target container.
			includeTestRepo = true
			frontend.Warn(ctx, client, worker, "Including test repo from worker image")
		}

		st := buildImageRootfs(worker, spec, sOpt, deb, targetKey, includeTestRepo, opt)

		def, err := st.Marshal(ctx)
		if err != nil {
			return nil, nil, err
		}

		img, err := buildImageConfig(ctx, client, spec, platform, targetKey)
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

		if err := frontend.RunTests(ctx, client, spec, ref, installTestDeps(worker, spec, targetKey, opt), targetKey); err != nil {
			return nil, nil, err
		}

		return ref, img, err
	})
}

func buildImageConfig(ctx context.Context, resolver llb.ImageMetaResolver, spec *dalec.Spec, platform *ocispecs.Platform, targetKey string) (*dalec.DockerImageSpec, error) {
	ref := dalec.GetBaseOutputImage(spec, targetKey)
	if ref == "" {
		ref = jammyRef
	}

	_, _, dt, err := resolver.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
		Platform: platform,
	})
	if err != nil {
		return nil, err
	}

	var i dalec.DockerImageSpec
	if err := json.Unmarshal(dt, &i); err != nil {
		return nil, errors.Wrap(err, "error unmarshalling base image config")
	}
	img := &i

	if err := dalec.BuildImageConfig(spec, targetKey, img); err != nil {
		return nil, err
	}

	return img, nil
}

func aptWorker(opts ...llb.ConstraintsOpt) llb.StateOption {
	return func(in llb.State) llb.State {
		return in.With(installPackages(dalec.WithConstraints(opts...), "apt-utils", "mmdebstrap"))
	}
}

func buildImageRootfs(worker llb.State, spec *dalec.Spec, sOpt dalec.SourceOpts, deb llb.State, targetKey string, includeTestRepo bool, opts ...llb.ConstraintsOpt) llb.State {
	base := dalec.GetBaseOutputImage(spec, targetKey)

	installSymlinks := func(in llb.State) llb.State {
		post := spec.GetImagePost(targetKey)
		if post == nil {
			return in
		}

		if len(post.Symlinks) == 0 {
			return in
		}

		const workPath = "/tmp/rootfs"
		return worker.
			Run(dalec.WithConstraints(opts...), dalec.InstallPostSymlinks(post, workPath)).
			AddMount(workPath, in)
	}

	if base == "" {
		base = jammyRef
	}

	baseImg := llb.Image(base, llb.WithMetaResolver(sOpt.Resolver))

	debug := llb.Scratch().File(llb.Mkfile("debug", 0o644, []byte(`debug=2`)), opts...)
	opts = append(opts, dalec.ProgressGroup("Install spec package"))
	return baseImg.Run(
		dalec.ShArgs("set -x; apt update && apt install -y /tmp/pkg/*.deb && exit 0; ls -lh /etc/apt/sources.list.d; ls -lh /etc/testrepo; mount; exit 42"),
		llb.AddEnv("DEBIAN_FRONTEND", "noninteractive"),
		llb.AddMount("/tmp/pkg", deb, llb.Readonly),
		dalec.WithMountedAptCache(AptCachePrefix),
		dalec.RunOptFunc(func(cfg *llb.ExecInfo) {
			if includeTestRepo {
				llb.AddMount(testRepoPath, worker, llb.SourcePath(testRepoPath)).SetRunOption(cfg)
				llb.AddMount(testRepoSourceListPath, worker, llb.SourcePath(testRepoSourceListPath)).SetRunOption(cfg)
			}
		}),
		dalec.WithConstraints(opts...),
		llb.AddMount("/etc/dpkg/dpkg.cfg.d/99-dalec-debug", debug, llb.SourcePath("debug"), llb.Readonly),
		dalec.RunOptFunc(func(cfg *llb.ExecInfo) {
			tmp := llb.Scratch().File(llb.Mkfile("tmp", 0o644, nil))
			// Warning: HACK here
			// The base ubuntu image has this `excludes` config file which prevents
			// installation of a lot of thigns, including doc files.
			// This is mounting over that file with an empty file so that our test suite
			// passes (as it is looking at these files).
			llb.AddMount("/etc/dpkg/dpkg.cfg.d/excludes", tmp, llb.SourcePath("tmp")).SetRunOption(cfg)
		}),
	).Root().
		With(installSymlinks)
}

// debstrap produces a command suitable for using mmdebstrap to install depdendencies.
// It is assumed that the location that debs should be installed to is /tmp/rootfs
// It is also assumed that the apt sources is at /etc/apt/sources.list
// This also removes all apt artifacts.
func debstrap(packages []string) llb.RunOption {
	return dalec.ShArgs("set -ex; apt-get update; mmdebstrap --debug --verbose --variant=essential --mode=chrootless --include=" + strings.Join(packages, ",") + " jammy /tmp/rootfs /etc/apt/sources.list; rm -rf /tmp/rootfs/var/lib/apt; rm -rf /tmp/rootfs/var/cache/apt")
}

func shouldIncludeSystemdDeps(spec *dalec.Spec) bool {
	if spec.Artifacts.Systemd.IsEmpty() {
		return false
	}
	return len(spec.Artifacts.Systemd.Units) > 0
}

func getPackageList(spec *dalec.Spec) []string {
	// pkgs := baseDeps
	var pkgs []string
	//if shouldIncludeSystemdDeps(spec) {
	//	pkgs = append(pkgs, systemdDeps...)
	//}

	pkgs = append(pkgs, spec.Name)
	return pkgs
}

// bootstrapRootfs creates a rootfs from scratch using the built package and all its runtime dependencies
func bootstrapRootfs(worker llb.State, repoMount llb.RunOption, spec *dalec.Spec, opts ...llb.ConstraintsOpt) llb.State {
	opts = append(opts, dalec.ProgressGroup("Bootstrap rootfs"))
	return worker.
		Run(
			// llb.Security(llb.SecurityModeInsecure),
			debstrap(getPackageList(spec)),
			llb.AddEnv("DEBIAN_FRONTEND", "noninteractive"),
			repoMount,
			dalec.WithConstraints(opts...),
			dalec.WithMountedAptCache(AptCachePrefix),
		).
		AddMount("/tmp/rootfs", llb.Scratch())
}

func installTestDeps(worker llb.State, spec *dalec.Spec, targetKey string, opts ...llb.ConstraintsOpt) llb.StateOption {
	return func(in llb.State) llb.State {
		deps := spec.GetTestDeps(targetKey)
		if len(deps) == 0 {
			return in
		}

		opts = append(opts, dalec.ProgressGroup("Install test dependencies"))

		noBaseImg := dalec.GetBaseOutputImage(spec, targetKey) == ""

		// When building the actual container we differentiate whether or not to use
		// mmdebstrap or straight apt-get based on wehter or not the spec includes a
		// custom base image to use.
		// It ia assumed that if a custom base is provided then it is able to use apt-get.
		// Since we are installing more packages to the container image we'll use that same logic here.
		if noBaseImg {
			worker := worker.With(aptWorker(opts...))
			return worker.Run(
				debstrap(deps),
				llb.Security(llb.SecurityModeInsecure),
				llb.AddEnv("DEBIAN_FRONTEND", "noninteractive"),
			).
				AddMount("/tmp/rootfs", in)
		}

		return in.Run(
			dalec.ShArgs("apt-get update && apt-get install -y --no-install-recommends "+strings.Join(deps, " ")),
			llb.AddEnv("DEBIAN_FRONTEND", "noninteractive"),
			dalec.WithMountedAptCache(AptCachePrefix),
		).Root()
	}
}