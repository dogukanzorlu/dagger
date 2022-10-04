package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"go.dagger.io/dagger/core/schema"
	"go.dagger.io/dagger/core/shim"
	"go.dagger.io/dagger/router"
)

// Container is a content-addressed container.
type Container struct {
	ID ContainerID `json:"id"`
}

// ContainerAddress is a container image address.
type ContainerAddress string

// ContainerID is an opaque value representing a content-addressed container.
type ContainerID string

func (id ContainerID) decode() (*containerIDPayload, error) {
	if id == "" {
		// scratch
		return &containerIDPayload{}, nil
	}

	var payload containerIDPayload
	if err := decodeID(&payload, id); err != nil {
		return nil, err
	}

	return &payload, nil
}

// containerIDPayload is the inner content of a ContainerID.
type containerIDPayload struct {
	// The container's root filesystem.
	FS *pb.Definition `json:"fs"`

	// Image configuration (env, workdir, etc)
	Config specs.ImageConfig `json:"cfg"`

	// Mount points configured for the container.
	Mounts []ContainerMount `json:"mounts,omitempty"`

	// Meta is the /dagger filesystem. It will be null if nothing has run yet.
	Meta *pb.Definition `json:"meta,omitempty"`
}

// Encode returns the opaque string ID representation of the container.
func (payload *containerIDPayload) Encode() (ContainerID, error) {
	id, err := encodeID(payload)
	if err != nil {
		return "", err
	}

	return ContainerID(id), nil
}

// FSState returns the container's root filesystem mount state. If there is
// none (as with an empty container ID), it returns scratch.
func (payload *containerIDPayload) FSState() (llb.State, error) {
	if payload.FS == nil {
		return llb.Scratch(), nil
	}

	return defToState(payload.FS)
}

// metaMount is the special path that the shim writes metadata to.
const metaMount = "/dagger"

// MetaState returns the container's metadata mount state. If the container has
// yet to run, it returns nil.
func (payload *containerIDPayload) MetaState() (*llb.State, error) {
	if payload.Meta == nil {
		return nil, nil
	}

	metaSt, err := defToState(payload.Meta)
	if err != nil {
		return nil, err
	}

	return &metaSt, nil
}

// ContainerMount is a mount point configured in a container.
type ContainerMount struct {
	// The source of the mount.
	Source *pb.Definition `json:"source"`

	// A path beneath the source to scope the mount to.
	SourcePath string `json:"source_path,omitempty"`

	// The path of the mount within the container.
	Target string `json:"target"`
}

// SourceState returns the state of the source of the mount.
func (mnt ContainerMount) SourceState() (llb.State, error) {
	return defToState(mnt.Source)
}

func (container *Container) FS(ctx context.Context) (*Directory, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, err
	}

	st, err := payload.FSState()
	if err != nil {
		return nil, err
	}

	return NewDirectory(ctx, st, "")
}

func (container *Container) WithFS(ctx context.Context, st llb.State, platform specs.Platform) (*Container, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, err
	}

	stDef, err := st.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, err
	}

	payload.FS = stDef.ToPB()

	id, err := payload.Encode()
	if err != nil {
		return nil, err
	}

	return &Container{ID: id}, nil
}

func (container *Container) WithMountedDirectory(ctx context.Context, target string, source *Directory) (*Container, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, err
	}

	dirSt, dirRel, err := source.Decode()
	if err != nil {
		return nil, err
	}

	dirDef, err := dirSt.Marshal(ctx)
	if err != nil {
		return nil, err
	}

	payload.Mounts = append(payload.Mounts, ContainerMount{
		Source:     dirDef.ToPB(),
		SourcePath: dirRel,
		Target:     target,
	})

	id, err := payload.Encode()
	if err != nil {
		return nil, err
	}

	return &Container{ID: id}, nil
}

func (container *Container) ImageConfig(ctx context.Context) (specs.ImageConfig, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return specs.ImageConfig{}, err
	}

	return payload.Config, nil
}

func (container *Container) UpdateImageConfig(ctx context.Context, updateFn func(specs.ImageConfig) specs.ImageConfig) (*Container, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, err
	}

	payload.Config = updateFn(payload.Config)

	id, err := payload.Encode()
	if err != nil {
		return nil, err
	}

	return &Container{ID: id}, nil
}

func (container *Container) Exec(ctx context.Context, gw bkgw.Client, platform specs.Platform, args []string, opts ContainerExecOpts) (*Container, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, fmt.Errorf("decode id: %w", err)
	}

	cfg := payload.Config
	mounts := payload.Mounts

	shimSt, err := shim.Build(ctx, gw, platform)
	if err != nil {
		return nil, fmt.Errorf("build shim: %w", err)
	}

	runOpts := []llb.RunOption{
		// run the command via the shim, hide shim behind custom name
		llb.AddMount(shim.Path, shimSt, llb.SourcePath(shim.Path)),
		llb.Args(append([]string{shim.Path}, args...)),
		llb.WithCustomName(strings.Join(args, " ")),
		llb.AddMount(metaMount, llb.Scratch()),
	}

	if cfg.WorkingDir != "" {
		runOpts = append(runOpts, llb.Dir(cfg.WorkingDir))
	}

	for _, env := range cfg.Env {
		name, val, ok := strings.Cut(env, "=")
		if !ok {
			// it's OK to not be OK
			// we'll just set an empty env
			_ = ok
		}

		runOpts = append(runOpts, llb.AddEnv(name, val))
	}

	for _, mnt := range mounts {
		st, err := mnt.SourceState()
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", mnt.Target, err)
		}

		mountOpts := []llb.MountOption{}
		if mnt.SourcePath != "" {
			mountOpts = append(mountOpts, llb.SourcePath(mnt.SourcePath))
		}

		runOpts = append(runOpts, llb.AddMount(mnt.Target, st, mountOpts...))
	}

	st, err := payload.FSState()
	if err != nil {
		return nil, fmt.Errorf("fs state: %w", err)
	}

	execSt := st.Run(runOpts...)

	execDef, err := execSt.Root().Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, fmt.Errorf("marshal root: %w", err)
	}

	// propagate any changes to the mounts to subsequent containers
	for i, mnt := range mounts {
		execMountDef, err := execSt.GetMount(mnt.Target).Marshal(ctx, llb.Platform(platform))
		if err != nil {
			return nil, fmt.Errorf("propagate %s: %w", mnt.Target, err)
		}

		mounts[i].Source = execMountDef.ToPB()
	}

	metaDef, err := execSt.GetMount(metaMount).Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, fmt.Errorf("get meta mount: %w", err)
	}

	payload.FS = execDef.ToPB()
	payload.Mounts = mounts
	payload.Meta = metaDef.ToPB()

	id, err := payload.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	return &Container{ID: id}, nil
}

func (container *Container) ExitCode(ctx context.Context, gw bkgw.Client) (*int, error) {
	file, err := container.MetaFile(ctx, gw, "exitCode")
	if err != nil {
		return nil, err
	}

	if file != nil {
		return nil, nil
	}

	content, err := file.Contents(ctx, gw)
	if err != nil {
		return nil, err
	}

	exitCode, err := strconv.Atoi(string(content))
	if err != nil {
		return nil, err
	}

	return &exitCode, nil
}

func (container *Container) MetaFile(ctx context.Context, gw bkgw.Client, path string) (*File, error) {
	payload, err := container.ID.decode()
	if err != nil {
		return nil, err
	}

	meta, err := payload.MetaState()
	if err != nil {
		return nil, err
	}

	if meta == nil {
		return nil, nil
	}

	return NewFile(ctx, *meta, path)
}

type containerSchema struct {
	*baseSchema
}

var _ router.ExecutableSchema = &containerSchema{}

func (s *containerSchema) Name() string {
	return "container"
}

func (s *containerSchema) Schema() string {
	return schema.Container
}

func (s *containerSchema) Resolvers() router.Resolvers {
	return router.Resolvers{
		"ContainerID":      stringResolver(ContainerID("")),
		"ContainerAddress": stringResolver(ContainerAddress("")),
		"Query": router.ObjectResolver{
			"container": router.ToResolver(s.container),
		},
		"Container": router.ObjectResolver{
			"from":                 router.ToResolver(s.from),
			"rootfs":               router.ToResolver(s.rootfs),
			"directory":            router.ErrResolver(ErrNotImplementedYet),
			"user":                 router.ErrResolver(ErrNotImplementedYet),
			"withUser":             router.ErrResolver(ErrNotImplementedYet),
			"workdir":              router.ToResolver(s.workdir),
			"withWorkdir":          router.ToResolver(s.withWorkdir),
			"variables":            router.ToResolver(s.variables),
			"variable":             router.ErrResolver(ErrNotImplementedYet),
			"withVariable":         router.ToResolver(s.withVariable),
			"withSecretVariable":   router.ErrResolver(ErrNotImplementedYet),
			"withoutVariable":      router.ErrResolver(ErrNotImplementedYet),
			"entrypoint":           router.ErrResolver(ErrNotImplementedYet),
			"withEntrypoint":       router.ErrResolver(ErrNotImplementedYet),
			"mounts":               router.ErrResolver(ErrNotImplementedYet),
			"withMountedDirectory": router.ToResolver(s.withMountedDirectory),
			"withMountedFile":      router.ErrResolver(ErrNotImplementedYet),
			"withMountedTemp":      router.ErrResolver(ErrNotImplementedYet),
			"withMountedCache":     router.ErrResolver(ErrNotImplementedYet),
			"withMountedSecret":    router.ErrResolver(ErrNotImplementedYet),
			"withoutMount":         router.ErrResolver(ErrNotImplementedYet),
			"exec":                 router.ToResolver(s.exec),
			"exitCode":             router.ToResolver(s.exitCode),
			"stdout":               router.ToResolver(s.stdout),
			"stderr":               router.ToResolver(s.stderr),
			"publish":              router.ErrResolver(ErrNotImplementedYet),
		},
	}
}

func (s *containerSchema) Dependencies() []router.ExecutableSchema {
	return nil
}

type containerArgs struct {
	ID ContainerID
}

func (s *containerSchema) container(ctx *router.Context, parent any, args containerArgs) (*Container, error) {
	return &Container{
		ID: args.ID,
	}, nil
}

type containerFromArgs struct {
	Address ContainerAddress
}

func (s *containerSchema) from(ctx *router.Context, parent *Container, args containerFromArgs) (*Container, error) {
	addr := string(args.Address)

	refName, err := reference.ParseNormalizedNamed(addr)
	if err != nil {
		return nil, err
	}

	ref := reference.TagNameOnly(refName).String()

	_, cfgBytes, err := s.gw.ResolveImageConfig(ctx, ref, llb.ResolveImageConfigOpt{
		Platform:    &s.platform,
		ResolveMode: llb.ResolveModeDefault.String(),
	})
	if err != nil {
		return nil, err
	}

	var imgSpec specs.Image
	if err := json.Unmarshal(cfgBytes, &imgSpec); err != nil {
		return nil, err
	}

	ctr, err := parent.WithFS(ctx, llb.Image(addr), s.platform)
	if err != nil {
		return nil, err
	}

	return ctr.UpdateImageConfig(ctx, func(specs.ImageConfig) specs.ImageConfig {
		return imgSpec.Config
	})
}

func (s *containerSchema) rootfs(ctx *router.Context, parent *Container, args any) (*Directory, error) {
	return parent.FS(ctx)
}

type containerExecArgs struct {
	Args []string
	Opts ContainerExecOpts
}

type ContainerExecOpts struct {
	Stdin          *string
	RedirectStdout *string
	RedirectStderr *string
}

func (s *containerSchema) exec(ctx *router.Context, parent *Container, args containerExecArgs) (*Container, error) {
	return parent.Exec(ctx, s.gw, s.platform, args.Args, args.Opts)
}

func (s *containerSchema) exitCode(ctx *router.Context, parent *Container, args any) (*int, error) {
	return parent.ExitCode(ctx, s.gw)
}

func (s *containerSchema) stdout(ctx *router.Context, parent *Container, args any) (*File, error) {
	return parent.MetaFile(ctx, s.gw, "stdout")
}

func (s *containerSchema) stderr(ctx *router.Context, parent *Container, args any) (*File, error) {
	return parent.MetaFile(ctx, s.gw, "stderr")
}

type containerWithWorkdirArgs struct {
	Path string
}

func (s *containerSchema) withWorkdir(ctx *router.Context, parent *Container, args containerWithWorkdirArgs) (*Container, error) {
	return parent.UpdateImageConfig(ctx, func(cfg specs.ImageConfig) specs.ImageConfig {
		cfg.WorkingDir = args.Path
		return cfg
	})
}

func (s *containerSchema) workdir(ctx *router.Context, parent *Container, args containerWithVariableArgs) (string, error) {
	cfg, err := parent.ImageConfig(ctx)
	if err != nil {
		return "", err
	}

	return cfg.WorkingDir, nil
}

type containerWithVariableArgs struct {
	Name  string
	Value string
}

func (s *containerSchema) withVariable(ctx *router.Context, parent *Container, args containerWithVariableArgs) (*Container, error) {
	return parent.UpdateImageConfig(ctx, func(cfg specs.ImageConfig) specs.ImageConfig {
		// NB(vito): buildkit handles replacing properly when we do llb.AddEnv, but
		// we want to replace it here anyway because someone might publish the image
		// instead of running it. (there's a test covering this!)
		newEnv := []string{}
		prefix := args.Name + "="
		for _, env := range cfg.Env {
			if !strings.HasPrefix(env, prefix) {
				newEnv = append(newEnv, env)
			}
		}

		newEnv = append(newEnv, fmt.Sprintf("%s=%s", args.Name, args.Value))

		cfg.Env = newEnv

		return cfg
	})
}

func (s *containerSchema) variables(ctx *router.Context, parent *Container, args containerWithVariableArgs) ([]string, error) {
	cfg, err := parent.ImageConfig(ctx)
	if err != nil {
		return nil, err
	}

	return cfg.Env, nil
}

type containerWithMountedDirectoryArgs struct {
	Path   string
	Source DirectoryID
}

func (s *containerSchema) withMountedDirectory(ctx *router.Context, parent *Container, args containerWithMountedDirectoryArgs) (*Container, error) {
	return parent.WithMountedDirectory(ctx, args.Path, &Directory{ID: args.Source})
}