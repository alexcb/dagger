package core

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/dagger/dagger/engine/buildkit"
	"github.com/dagger/dagger/network"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/errdefs"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/progress/logs"
	"github.com/moby/buildkit/worker"
	"github.com/pkg/errors"
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

func (container *Container) WithExec(ctx context.Context, opts ContainerExecOpts) (*Container, error) { //nolint:gocyclo
	container = container.Clone()

	cfg := container.Config
	mounts := container.Mounts
	platform := container.Platform
	if platform.OS == "" {
		platform = container.Query.Platform()
	}

	args, err := container.command(opts)
	if err != nil {
		return nil, err
	}

	runOpts := []llb.RunOption{
		llb.Args(args),
		buildkit.WithTracePropagation(ctx),
		buildkit.WithPassthrough(),
	}

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
		// include an env var (which will be removed before the exec actually runs) so that execs with
		// inits disabled will be cached differently than those with them enabled by buildkit
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerNoInitEnv, "true"))
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

		// include the engine version so that these execs get invalidated if the engine/API change
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerEngineVersionEnv, engine.Version))
	}

	if execMD.CachePerSession {
		// include the SessionID here so that we bust cache once-per-session
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerSessionIDEnv, clientMetadata.SessionID))
	}

	if execMD.CacheByCall {
		// include a digest of the current call so that we scope of the cache of the ExecOp to this call's args
		// and receiver values. Currently only used for module function calls.
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerCallDigestEnv, string(dagql.CurrentID(ctx).Digest())))
	}

	metaSt, metaSourcePath := metaMount(ctx, opts.Stdin)

	// create mount point for the executor to write stdout/stderr/exitcode to
	runOpts = append(runOpts,
		llb.AddMount(buildkit.MetaMountDestPath, metaSt, llb.SourcePath(metaSourcePath)))

	if opts.RedirectStdout != "" {
		// ensure this path is in the cache key
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerRedirectStdoutEnv, opts.RedirectStdout))
	}

	if opts.RedirectStderr != "" {
		// ensure this path is in the cache key
		runOpts = append(runOpts, llb.AddEnv(buildkit.DaggerRedirectStderrEnv, opts.RedirectStderr))
	}

	var aliasStrs []string
	for _, bnd := range container.Services {
		for _, alias := range bnd.Aliases {
			execMD.HostAliases[bnd.Hostname] = append(execMD.HostAliases[bnd.Hostname], alias)
			aliasStrs = append(aliasStrs, bnd.Hostname+"="+alias)
		}
	}
	if len(aliasStrs) > 0 {
		// ensure these are in the cache key, sort them for stability
		slices.Sort(aliasStrs)
		runOpts = append(runOpts,
			llb.AddEnv(buildkit.DaggerHostnameAliasesEnv, strings.Join(aliasStrs, ",")))
	}

	if cfg.User != "" {
		runOpts = append(runOpts, llb.User(cfg.User))
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

	for i, secret := range container.Secrets {
		secretOpts := []llb.SecretOption{llb.SecretID(secret.Secret.ID().Digest().String())}

		var secretDest string
		switch {
		case secret.EnvName != "":
			secretDest = secret.EnvName
			secretOpts = append(secretOpts, llb.SecretAsEnv(true))
			execMD.SecretEnvNames = append(execMD.SecretEnvNames, secret.EnvName)
		case secret.MountPath != "":
			secretDest = secret.MountPath
			execMD.SecretFilePaths = append(execMD.SecretFilePaths, secret.MountPath)
			if secret.Owner != nil {
				secretOpts = append(secretOpts, llb.SecretFileOpt(
					secret.Owner.UID,
					secret.Owner.GID,
					int(secret.Mode),
				))
			}
		default:
			return nil, fmt.Errorf("malformed secret config at index %d", i)
		}

		runOpts = append(runOpts, llb.AddSecret(secretDest, secretOpts...))
	}

	for _, ctrSocket := range container.Sockets {
		if ctrSocket.ContainerPath == "" {
			return nil, fmt.Errorf("unsupported socket: only unix paths are implemented")
		}

		socketOpts := []llb.SSHOption{
			llb.SSHID(ctrSocket.Source.LLBID()),
			llb.SSHSocketTarget(ctrSocket.ContainerPath),
		}

		if ctrSocket.Owner != nil {
			socketOpts = append(socketOpts,
				llb.SSHSocketOpt(
					ctrSocket.ContainerPath,
					ctrSocket.Owner.UID,
					ctrSocket.Owner.GID,
					0o600, // preserve default
				))
		}

		runOpts = append(runOpts, llb.AddSSHSocket(socketOpts...))
	}

	for _, mnt := range mounts {
		srcSt, err := mnt.SourceState()
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", mnt.Target, err)
		}

		mountOpts := []llb.MountOption{}

		if mnt.SourcePath != "" {
			mountOpts = append(mountOpts, llb.SourcePath(mnt.SourcePath))
		}

		if mnt.CacheVolumeID != "" {
			var sharingMode llb.CacheMountSharingMode
			switch mnt.CacheSharingMode {
			case CacheSharingModeShared:
				sharingMode = llb.CacheMountShared
			case CacheSharingModePrivate:
				sharingMode = llb.CacheMountPrivate
			case CacheSharingModeLocked:
				sharingMode = llb.CacheMountLocked
			default:
				return nil, errors.Errorf("invalid cache mount sharing mode %q", mnt.CacheSharingMode)
			}

			mountOpts = append(mountOpts, llb.AsPersistentCacheDir(mnt.CacheVolumeID, sharingMode))
		}

		if mnt.Tmpfs {
			mountOpts = append(mountOpts, llb.Tmpfs(llb.TmpfsSize(int64(mnt.Size))))
		}

		if mnt.Readonly {
			mountOpts = append(mountOpts, llb.Readonly)
		}

		runOpts = append(runOpts, llb.AddMount(mnt.Target, srcSt, mountOpts...))
	}

	if opts.Expect != ReturnSuccess {
		runOpts = append(runOpts, llb.ValidExitCodes(opts.Expect.ReturnCodes()...))
	}

	if opts.InsecureRootCapabilities {
		runOpts = append(runOpts, llb.Security(llb.SecurityModeInsecure))
	}

	fsSt, err := container.FSState()
	if err != nil {
		return nil, fmt.Errorf("fs state: %w", err)
	}

	execMDOpt, err := execMD.AsConstraintsOpt()
	if err != nil {
		return nil, fmt.Errorf("execution metadata: %w", err)
	}
	runOpts = append(runOpts, execMDOpt)
	execSt := fsSt.Run(runOpts...)

	marshalOpts := []llb.ConstraintsOpt{
		llb.Platform(platform.Spec()),
		execMDOpt,
	}
	if opts.ExperimentalPrivilegedNesting {
		marshalOpts = append(marshalOpts, llb.SkipEdgeMerge)
	}
	execDef, err := execSt.Root().Marshal(ctx, marshalOpts...)
	if err != nil {
		return nil, fmt.Errorf("marshal root: %w", err)
	}

	container.FS = execDef.ToPB()

	metaDef, err := execSt.GetMount(buildkit.MetaMountDestPath).Marshal(ctx, marshalOpts...)
	if err != nil {
		return nil, fmt.Errorf("get meta mount: %w", err)
	}

	container.Meta = metaDef.ToPB()

	for i, mnt := range mounts {
		if mnt.Tmpfs || mnt.CacheVolumeID != "" {
			continue
		}

		mountSt := execSt.GetMount(mnt.Target)

		// propagate any changes to regular mounts to subsequent containers
		execMountDef, err := mountSt.Marshal(ctx, marshalOpts...)
		if err != nil {
			return nil, fmt.Errorf("propagate %s: %w", mnt.Target, err)
		}

		mounts[i].Source = execMountDef.ToPB()
	}

	container.Mounts = mounts

	// set image ref to empty string
	container.ImageRef = ""

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
	// exec, ok := dag.AsExec()
	// if !ok {
	// 	return nil, fmt.Errorf("expected exec")
	// }
	// execop := ops.NewExecOp(vtx, exec.ExecOp, exec.Platform, container.Query.BuildkitCache(), nil, sessionmanager, exec, worker)

	// TODO:
	// - immediately unmarshal llb, and run, see ExecOp.Exec
	// - copy paste code through, remove llb piece-by-piece (e.g. so instead of
	//   reading the mounts directly out of the llb, just create executor.Mounts
	//   directly)
	// - keep going until no more llb read/writes, and remove LLB conversion
	// - keep worker abstraction... for now. need to discuss with sipsma. may break dockerfile builds.

	// --------------------------------------------------------------------------------

	op, ok := DagOpFromContext[ContainerDagOp](ctx)
	if !ok {
		return nil, fmt.Errorf("no dagop")
	}

	refs := op.GetMounts()
	workerRefs := make([]worker.WorkerRef, 0, len(refs))
	for _, ref := range refs {
		// XXX: PrepareMounts needs this (but only uses ImmutableRef)
		workerRefs = append(workerRefs, worker.WorkerRef{ImmutableRef: ref})
	}

	platformOS := runtime.GOOS
	// if e.platform != nil {
	// 	platformOS = e.platform.OS
	// }
	p, err := container.PrepareMounts(ctx, e.mm, e.cm, g, e.op.Meta.Cwd, e.op.Mounts, refs, func(m *pb.Mount, ref cache.ImmutableRef) (cache.MutableRef, error) {
		desc := fmt.Sprintf("mount %s from exec %s", m.Dest, strings.Join(e.op.Meta.Args, " "))
		return e.cm.New(ctx, ref, g, cache.WithDescription(desc))
	}, platformOS)
	defer func() {
		if err != nil {
			execInputs := make([]solver.Result, len(e.op.Mounts))
			for i, m := range e.op.Mounts {
				if m.Input == -1 {
					continue
				}
				execInputs[i] = inputs[m.Input].Clone()
			}
			execMounts := make([]solver.Result, len(e.op.Mounts))
			copy(execMounts, execInputs)
			for i, res := range results {
				execMounts[p.OutputRefs[i].MountIndex] = res
			}
			for _, active := range p.Actives {
				if active.NoCommit {
					active.Ref.Release(context.TODO())
				} else {
					ref, cerr := active.Ref.Commit(ctx)
					if cerr != nil {
						err = errors.Wrapf(err, "error committing %s: %s", active.Ref.ID(), cerr)
						continue
					}
					execMounts[active.MountIndex] = worker.NewWorkerRefResult(ref, e.w)
				}
			}
			err = errdefs.WithExecError(err, execInputs, execMounts)
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

	extraHosts, err := container.ParseExtraHosts(e.op.Meta.ExtraHosts)
	if err != nil {
		return nil, err
	}

	emu, err := getEmulator(ctx, e.platform)
	if err != nil {
		return nil, err
	}
	if emu != nil {
		e.op.Meta.Args = append([]string{qemuMountName}, e.op.Meta.Args...)

		p.Mounts = append(p.Mounts, executor.Mount{
			Readonly: true,
			Src:      emu,
			Dest:     qemuMountName,
		})
	}

	meta := executor.Meta{
		Args:                      e.op.Meta.Args,
		Env:                       e.op.Meta.Env,
		Cwd:                       e.op.Meta.Cwd,
		User:                      e.op.Meta.User,
		Hostname:                  e.op.Meta.Hostname,
		ReadonlyRootFS:            p.ReadonlyRootFS,
		ExtraHosts:                extraHosts,
		Ulimit:                    e.op.Meta.Ulimit,
		CgroupParent:              e.op.Meta.CgroupParent,
		NetMode:                   e.op.Network,
		SecurityMode:              e.op.Security,
		RemoveMountStubsRecursive: e.op.Meta.RemoveMountStubsRecursive,
	}

	if e.op.Meta.ProxyEnv != nil {
		meta.Env = append(meta.Env, proxyEnvList(e.op.Meta.ProxyEnv)...)
	}
	var currentOS string
	if e.platform != nil {
		currentOS = e.platform.OS
	}
	meta.Env = addDefaultEnvvar(meta.Env, "PATH", utilsystem.DefaultPathEnv(currentOS))

	secretEnv, err := e.loadSecretEnv(ctx, g)
	if err != nil {
		return nil, err
	}
	meta.Env = append(meta.Env, secretEnv...)

	if e.op.Meta.ValidExitCodes != nil {
		meta.ValidExitCodes = make([]int, len(e.op.Meta.ValidExitCodes))
		for i, code := range e.op.Meta.ValidExitCodes {
			meta.ValidExitCodes[i] = int(code)
		}
	}

	stdout, stderr, flush := logs.NewLogStreams(ctx, os.Getenv("BUILDKIT_DEBUG_EXEC_OUTPUT") == "1")
	defer stdout.Close()
	defer stderr.Close()
	defer func() {
		if err != nil {
			flush()
		}
	}()

	rec, execErr := e.exec.Run(ctx, "", p.Root, p.Mounts, executor.ProcessInfo{
		Meta:   meta,
		Stdin:  nil,
		Stdout: stdout,
		Stderr: stderr,
	}, nil)

	for i, out := range p.OutputRefs {
		if mutable, ok := out.Ref.(cache.MutableRef); ok {
			ref, err := mutable.Commit(ctx)
			if err != nil {
				return nil, errors.Wrapf(err, "error committing %s", mutable.ID())
			}
			results = append(results, worker.NewWorkerRefResult(ref, e.w))
		} else {
			results = append(results, worker.NewWorkerRefResult(out.Ref.(cache.ImmutableRef), e.w))
		}
		// Prevent the result from being released.
		p.OutputRefs[i].Ref = nil
	}
	e.rec = rec

	return container, nil
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
