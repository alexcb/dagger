package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	bkcache "github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/executor"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	bkcontainer "github.com/moby/buildkit/frontend/gateway/container"
	"github.com/moby/buildkit/identity"
	bksession "github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	bksolver "github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/errdefs"
	bkmounts "github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/progress/logs"
	utilsystem "github.com/moby/buildkit/util/system"
	"github.com/moby/buildkit/worker"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/dagger/dagger/engine/buildkit"
	"github.com/dagger/dagger/network"
)

var ErrNoCommand = errors.New("no command has been set")
var ErrNoSvcCommand = errors.New("no service command has been set")

type ContainerExecOpts struct {
	// Command to run instead of the container's default command
	Args []string

	// If the container has an entrypoint, prepend it to this exec's args
	UseEntrypoint bool `default:"false"`

	// Content to write to the command's standard input before closing
	Stdin string `default:""`

	// Redirect the command's standard output to a file in the container
	RedirectStdout string `default:""`

	// Redirect the command's standard error to a file in the container
	RedirectStderr string `default:""`

	// Exit codes this exec is allowed to exit with
	Expect ReturnTypes `default:"SUCCESS"`

	// Provide the executed command access back to the Dagger API
	ExperimentalPrivilegedNesting bool `default:"false"`

	// Grant the process all root capabilities
	InsecureRootCapabilities bool `default:"false"`

	// (Internal-only) If this is a nested exec, exec metadata to use for it
	NestedExecMetadata *buildkit.ExecutionMetadata `name:"-"`

	// Expand the environment variables in args
	Expand bool `default:"false"`

	// Skip the init process injected into containers by default so that the
	// user's process is PID 1
	NoInit bool `default:"false"`
}

func (container *Container) getAllMounts() (mounts []*pb.Mount, inputs []llb.State, _ error) {
	addMount := func(mnt ContainerMount) error {
		st, err := defToState(mnt.Source)
		if err != nil {
			return err
		}

		// XXX: dedupl states

		mount := &pb.Mount{
			Dest:     mnt.Target,
			Selector: mnt.SourcePath,
			Output:   pb.OutputIndex(len(mounts)),
			Readonly: mnt.Readonly,
			// XXX: ?
			// ContentCache: nil,
		}
		if st.Output() == nil {
			mount.Input = pb.Empty
		} else {
			mount.Input = pb.InputIndex(len(inputs))
		}

		if mnt.CacheVolumeID != "" {
			mount.MountType = pb.MountType_CACHE
			mount.CacheOpt = &pb.CacheOpt{
				ID: mnt.CacheVolumeID,
			}
			switch mnt.CacheSharingMode {
			case CacheSharingModeShared:
				mount.CacheOpt.Sharing = pb.CacheSharingOpt_SHARED
			case CacheSharingModePrivate:
				mount.CacheOpt.Sharing = pb.CacheSharingOpt_PRIVATE
			case CacheSharingModeLocked:
				mount.CacheOpt.Sharing = pb.CacheSharingOpt_LOCKED
			}
		}

		if mnt.Tmpfs {
			mount.MountType = pb.MountType_TMPFS
			mount.TmpfsOpt = &pb.TmpfsOpt{
				Size_: int64(mnt.Size),
			}
		}

		mounts = append(mounts, mount)
		inputs = append(inputs, st)

		return nil
	}

	if err := addMount(ContainerMount{Source: container.FS, Target: "/"}); err != nil {
		return nil, nil, err
	}
	if err := addMount(ContainerMount{Source: container.Meta, Target: buildkit.MetaMountDestPath, SourcePath: buildkit.MetaMountDestPath}); err != nil {
		return nil, nil, err
	}
	for _, mount := range container.Mounts {
		if err := addMount(mount); err != nil {
			return nil, nil, err
		}
	}

	for _, secret := range container.Secrets {
		if secret.MountPath == "" {
			continue
		}

		uid, gid := 0, 0
		if secret.Owner != nil {
			uid, gid = secret.Owner.UID, secret.Owner.GID
		}
		mount := &pb.Mount{
			Input:     pb.Empty,
			Dest:      secret.MountPath,
			MountType: pb.MountType_SECRET,
			SecretOpt: &pb.SecretOpt{
				ID:   secret.Secret.ID().Digest().String(),
				Uid:  uint32(uid),
				Gid:  uint32(gid),
				Mode: uint32(secret.Mode),
			},
		}
		mounts = append(mounts, mount)
	}

	for _, socket := range container.Sockets {
		if socket.ContainerPath == "" {
			return nil, nil, fmt.Errorf("unsupported socket: only unix paths are implemented")
		}

		uid, gid := 0, 0
		if socket.Owner != nil {
			uid, gid = socket.Owner.UID, socket.Owner.GID
		}
		mount := &pb.Mount{
			Input:     pb.Empty,
			Dest:      socket.ContainerPath,
			MountType: pb.MountType_SSH,
			SSHOpt: &pb.SSHOpt{
				ID:  socket.Source.LLBID(),
				Uid: uint32(uid),
				Gid: uint32(gid),
				// Mode:     uint32(socket.Mode),
				Mode: 0o600, // preserve default
			},
		}
		mounts = append(mounts, mount)
	}

	return mounts, inputs, nil
}

func (container *Container) setAllMountStates(ctx context.Context, mounts []*pb.Mount, states []llb.State) error {
	for i, mount := range mounts {
		st := states[i]

		def, err := st.Marshal(ctx, llb.Platform(container.Platform.Spec()))
		if err != nil {
			return err
		}

		switch mount.Output {
		case 0:
			container.FS = def.ToPB()
		case 1:
			container.Meta = def.ToPB()
		default:
			container.Mounts[i-2].Source = def.ToPB()
		}
	}

	return nil
}

func (container *Container) getAllMountRefs(ctx context.Context, wkr worker.Worker, mounts []*pb.Mount) (outputs []bksolver.Result, _ error) {
	bk, err := container.Query.Buildkit(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get buildkit client: %w", err)
	}

	addRef := func(def *pb.Definition, ref bkcache.ImmutableRef) error {
		if ref != nil {
			outputs = append(outputs, worker.NewWorkerRefResult(ref.Clone(), wkr))
		} else {
			res, err := bk.Solve(ctx, bkgw.SolveRequest{
				// Evaluate:   true,
				Definition: def,
			})
			if err != nil {
				return err
			}
			ref, err := res.Ref.Result(ctx)
			if err != nil {
				return err
			}
			outputs = append(outputs, ref)
		}
		return nil
	}

	for i, mount := range mounts {
		switch mount.Output {
		case 0:
			addRef(container.FS, container.FSResult)
		case 1:
			addRef(container.Meta, container.MetaResult)
		default:
			mnt := container.Mounts[i-2]
			addRef(mnt.Source, mnt.Result)
		}
	}

	return outputs, nil
}

func (container *Container) WithExec(ctx context.Context, opts ContainerExecOpts) (_ *Container, rerr error) { //nolint:gocyclo
	// container = container.Clone()
	//
	cfg := container.Config
	// mounts := container.Mounts
	// platform := container.Platform
	// if platform.OS == "" {
	// 	platform = container.Query.Platform()
	// }
	//
	args, err := container.command(opts)
	if err != nil {
		return nil, err
	}
	//
	// runOpts := []llb.RunOption{
	// 	llb.Args(args),
	// 	buildkit.WithTracePropagation(ctx),
	// 	buildkit.WithPassthrough(),
	// }
	//
	clientMetadata, err := engine.ClientMetadataFromContext(ctx)
	if err != nil {
		return nil, err
	}

	execMD := buildkit.ExecutionMetadata{}
	if opts.NestedExecMetadata != nil {
		execMD = *opts.NestedExecMetadata
	}
	execMD.CallID = dagql.CurrentID(ctx)
	execMD.CallerClientID = clientMetadata.ClientID
	execMD.ExecID = identity.NewID()
	execMD.SessionID = clientMetadata.SessionID
	execMD.AllowedLLMModules = clientMetadata.AllowedLLMModules
	if execMD.HostAliases == nil {
		execMD.HostAliases = make(map[string][]string)
	}
	execMD.RedirectStdoutPath = opts.RedirectStdout
	execMD.RedirectStderrPath = opts.RedirectStderr
	execMD.SystemEnvNames = container.SystemEnvNames
	execMD.EnabledGPUs = container.EnabledGPUs
	if opts.NoInit {
		execMD.NoInit = true
	}

	mod, err := container.Query.CurrentModule(ctx)
	if err == nil {
		if mod.InstanceID == nil {
			return nil, fmt.Errorf("current module has no instance ID")
		}
		// allow the exec to reach services scoped to the module that
		// installed it
		execMD.ExtraSearchDomains = append(execMD.ExtraSearchDomains,
			network.ModuleDomain(mod.InstanceID, clientMetadata.SessionID))
	}

	// if GPU parameters are set for this container pass them over:
	if len(execMD.EnabledGPUs) > 0 {
		if gpuSupportEnabled := os.Getenv("_EXPERIMENTAL_DAGGER_GPU_SUPPORT"); gpuSupportEnabled == "" {
			return nil, fmt.Errorf("GPU support is not enabled, set _EXPERIMENTAL_DAGGER_GPU_SUPPORT")
		}
	}

	// this allows executed containers to communicate back to this API
	if opts.ExperimentalPrivilegedNesting {
		// establish new client ID for the nested client
		if execMD.ClientID == "" {
			execMD.ClientID = identity.NewID()
		}
	}

	var aliasStrs []string
	for _, bnd := range container.Services {
		for _, alias := range bnd.Aliases {
			execMD.HostAliases[bnd.Hostname] = append(execMD.HostAliases[bnd.Hostname], alias)
			aliasStrs = append(aliasStrs, bnd.Hostname+"="+alias)
		}
	}

	op, ok := DagOpFromContext[ContainerDagOp](ctx)
	if !ok {
		return nil, fmt.Errorf("no dagop")
	}

	// edge, err := llbsolver.Load(ctx, container.FS, nil)
	// if err != nil {
	// 	return nil, err
	// }
	// vtx := edge.Vertex
	//
	// dag, err := buildkit.DefToDAG(container.FS)
	// if err != nil {
	// 	return nil, err
	// }
	// dag = dag.Inputs[0]
	// exec, ok := dag.AsExec()
	// if !ok {
	// 	return nil, fmt.Errorf("expected exec")
	// }
	// execop := ops.NewExecOp(vtx, exec.ExecOp, exec.Platform, container.Query.BuildkitCache(), nil, sessionmanager, exec, worker)

	// for _, mount := range exec.Mounts {
	// 	switch mount.Dest {
	// 	case "/":
	// 		mount.Input = 0
	// 	case buildkit.MetaMountDestPath:
	// 		mount.Input = 1
	// 	default:
	// 		mount.Input = pb.InputIndex(op.GetMountIdx(mount.Dest))
	// 	}
	// }

	// --------------------------------------------------------------------------------

	refs := op.GetMounts()
	workerRefs := make([]*worker.WorkerRef, 0, len(refs))
	for _, ref := range refs {
		// XXX: PrepareMounts needs this (but only uses ImmutableRef)
		workerRefs = append(workerRefs, &worker.WorkerRef{ImmutableRef: ref})
	}

	cache := container.Query.BuildkitCache()
	session := container.Query.BuildkitSession()

	platformOS := runtime.GOOS
	// if e.platform != nil {
	// 	platformOS = e.platform.OS
	// }

	mmname := fmt.Sprintf("exec %s", strings.Join(args, " "))
	mm := bkmounts.NewMountManager(mmname, cache, session)
	p, err := bkcontainer.PrepareMounts(ctx, mm, cache, op.Group(), cfg.WorkingDir, op.Mounts, workerRefs, func(m *pb.Mount, ref bkcache.ImmutableRef) (bkcache.MutableRef, error) {
		desc := fmt.Sprintf("mount %s from exec %s", m.Dest, strings.Join(args, " "))
		return cache.New(ctx, ref, op.Group(), bkcache.WithDescription(desc))
	}, platformOS)
	defer func() {
		if rerr != nil {
			execInputs := make([]bksolver.Result, len(op.Mounts))
			for i, m := range op.Mounts {
				if m.Input == -1 {
					continue
				}
				execInputs[i] = op.inputs[m.Input].Clone()
			}
			execMounts := make([]bksolver.Result, len(op.Mounts))
			copy(execMounts, execInputs)
			results, err := container.getAllMountRefs(ctx, op.opt.Worker, op.Mounts)
			if err != nil {
				return
			}
			for i, res := range results {
				execMounts[p.OutputRefs[i].MountIndex] = res
			}
			for _, active := range p.Actives {
				if active.NoCommit {
					active.Ref.Release(context.TODO())
				} else {
					ref, cerr := active.Ref.Commit(ctx)
					if cerr != nil {
						rerr = fmt.Errorf("error committing %s: %w: %w", active.Ref.ID(), cerr, err)
						continue
					}
					execMounts[active.MountIndex] = worker.NewWorkerRefResult(ref, op.opt.Worker)
				}
			}

			// XXX: interactive isn't working :(
			rerr = errdefs.WithExecError(rerr, execInputs, execMounts)
		} else {
			// Only release actives if err is nil.
			for i := len(p.Actives) - 1; i >= 0; i-- { // call in LIFO order
				p.Actives[i].Ref.Release(context.TODO())
			}
		}
		for _, o := range p.OutputRefs {
			if o.Ref != nil {
				o.Ref.Release(context.TODO())
			}
		}
	}()
	if err != nil {
		return nil, err
	}

	// extraHosts, err := bkcontainer.ParseExtraHosts(e.op.Meta.ExtraHosts)
	// if err != nil {
	// 	return nil, err
	// }

	// emu, err := getEmulator(ctx, e.platform)
	// if err != nil {
	// 	return nil, err
	// }
	// if emu != nil {
	// 	e.op.Meta.Args = append([]string{qemuMountName}, e.op.Meta.Args...)
	//
	// 	p.Mounts = append(p.Mounts, executor.Mount{
	// 		Readonly: true,
	// 		Src:      emu,
	// 		Dest:     qemuMountName,
	// 	})
	// }

	meta := executor.Meta{
		Args: args,
		Env:  slices.Clone(cfg.Env), // XXX: + more
		Cwd:  cmp.Or(cfg.WorkingDir, "/"),
		User: cfg.User,
		// Hostname:       e.op.Meta.Hostname, // empty seems right?
		ReadonlyRootFS: p.ReadonlyRootFS,
		// ExtraHosts:     extraHosts,
		// Ulimit:                    e.op.Meta.Ulimit,
		// CgroupParent:              e.op.Meta.CgroupParent,
		// NetMode:                   e.op.Network,
		RemoveMountStubsRecursive: true,
	}
	if opts.InsecureRootCapabilities {
		meta.SecurityMode = pb.SecurityMode_INSECURE
	}

	// if e.op.Meta.ProxyEnv != nil {
	// 	meta.Env = append(meta.Env, proxyEnvList(e.op.Meta.ProxyEnv)...)
	// }
	meta.Env = addDefaultEnvvar(meta.Env, "PATH", utilsystem.DefaultPathEnv(platformOS))

	secretEnvs := []*pb.SecretEnv{}
	for i, secret := range container.Secrets {
		if secret.EnvName == "" {
			continue
		}

		switch {
		case secret.EnvName != "":
			secretEnvs = append(secretEnvs, &pb.SecretEnv{
				ID:   secret.Secret.ID().Digest().String(),
				Name: secret.EnvName,
			})
			execMD.SecretEnvNames = append(execMD.SecretEnvNames, secret.EnvName)
		case secret.MountPath != "":
			execMD.SecretFilePaths = append(execMD.SecretFilePaths, secret.MountPath)
		default:
			return nil, fmt.Errorf("malformed secret config at index %d", i)
		}
	}
	secretEnv, err := loadSecretEnv(ctx, op.Group(), session, secretEnvs)
	if err != nil {
		return nil, err
	}
	meta.Env = append(meta.Env, secretEnv...)

	if opts.Expect != ReturnSuccess {
		meta.ValidExitCodes = opts.Expect.ReturnCodes()
	}

	stdout, stderr, flush := logs.NewLogStreams(ctx, os.Getenv("BUILDKIT_DEBUG_EXEC_OUTPUT") == "1")
	defer stdout.Close()
	defer stderr.Close()
	defer func() {
		if err != nil {
			flush()
		}
	}()

	// FIXME: this abstraction is now irrelevant - we don't need to do buildkit smuggling anymore
	worker := op.opt.Worker.(*buildkit.Worker)
	worker = worker.ExecWorker(trace.SpanContextFromContext(ctx), execMD)
	exec := worker.Executor()
	_, execErr := exec.Run(ctx, "", p.Root, p.Mounts, executor.ProcessInfo{
		Meta:   meta,
		Stdin:  nil,
		Stdout: stdout,
		Stderr: stderr,
	}, nil)

	for i, out := range p.OutputRefs {
		var ref bkcache.ImmutableRef
		if mutable, ok := out.Ref.(bkcache.MutableRef); ok {
			ref, err = mutable.Commit(ctx)
			if err != nil {
				return nil, fmt.Errorf("error committing %s: %w", mutable.ID(), err)
			}
		} else {
			ref = out.Ref.(bkcache.ImmutableRef)
		}

		mount := op.Mounts[out.MountIndex]
		switch mount.Dest {
		case "/":
			fmt.Println("writing back fs")
			container.FSResult = ref
		case buildkit.MetaMountDestPath:
			fmt.Println("writing back meta")
			container.MetaResult = ref
		default:
			fmt.Println("writing back mount", i-2)
			container.Mounts[i-2].Result = ref
		}

		// Prevent the result from being released.
		p.OutputRefs[i].Ref = nil
	}

	if execErr != nil {
		return nil, fmt.Errorf("process %q did not complete successfully: %w", strings.Join(args, " "), execErr)
	}

	return container, nil
}

func addDefaultEnvvar(env []string, k, v string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, k+"=") {
			return env
		}
	}
	return append(env, k+"="+v)
}

// XXX:
func loadSecretEnv(ctx context.Context, g bksession.Group, sm *bksession.Manager, secretenv []*pb.SecretEnv) ([]string, error) {
	if len(secretenv) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(secretenv))
	eg, gctx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	for _, sopt := range secretenv {
		id := sopt.ID
		eg.Go(func() error {
			if id == "" {
				return fmt.Errorf("secret ID missing for %q environment variable", sopt.Name)
			}
			var dt []byte
			var err error
			err = sm.Any(gctx, g, func(ctx context.Context, _ string, caller bksession.Caller) error {
				dt, err = secrets.GetSecret(ctx, caller, id)
				if err != nil {
					if errors.Is(err, secrets.ErrNotFound) && sopt.Optional {
						return nil
					}
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, fmt.Sprintf("%s=%s", sopt.Name, string(dt)))
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

func (container *Container) Stdout(ctx context.Context) (string, error) {
	return container.metaFileContents(ctx, buildkit.MetaMountStdoutPath)
}

func (container *Container) Stderr(ctx context.Context) (string, error) {
	return container.metaFileContents(ctx, buildkit.MetaMountStderrPath)
}

func (container *Container) ExitCode(ctx context.Context) (int, error) {
	contents, err := container.metaFileContents(ctx, buildkit.MetaMountExitCodePath)
	if err != nil {
		return 0, err
	}
	contents = strings.TrimSpace(contents)

	code, err := strconv.ParseInt(contents, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("could not parse exit code %q: %w", contents, err)
	}

	return int(code), nil
}

func (container *Container) usedClientID(ctx context.Context) (string, error) {
	return container.metaFileContents(ctx, buildkit.MetaMountClientIDPath)
}

func (container *Container) metaFileContents(ctx context.Context, filePath string) (string, error) {
	if container.Meta == nil {
		return "", fmt.Errorf("%w: %s requires an exec", ErrNoCommand, filePath)
	}

	// fmt.Println("here!", filePath)
	// fmt.Println(container.Meta)

	file := NewFile(
		container.Query,
		container.Meta,
		path.Join(buildkit.MetaMountDestPath, filePath),
		container.Platform,
		container.Services,
	)

	content, err := file.Contents(ctx)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

func metaMount(ctx context.Context, stdin string) (llb.State, string) {
	meta := llb.Mkdir(buildkit.MetaMountDestPath, 0o777)
	if stdin != "" {
		meta = meta.Mkfile(path.Join(buildkit.MetaMountDestPath, buildkit.MetaMountStdinPath), 0o666, []byte(stdin))
	}

	return llb.Scratch().File(
			meta,
			buildkit.WithTracePropagation(ctx),
			buildkit.WithPassthrough(),
		),
		buildkit.MetaMountDestPath
}
