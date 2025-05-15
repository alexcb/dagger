package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/dagger/dagger/engine/buildkit"
	bkcache "github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	bksession "github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/worker"
	"github.com/opencontainers/go-digest"
)

func init() {
	buildkit.RegisterCustomOp(FSDagOp{})
	buildkit.RegisterCustomOp(RawDagOp{})
	buildkit.RegisterCustomOp(ContainerDagOp{})
}

// NewDirectoryDagOp takes a target ID for a Directory, and returns a Directory
// for it, computing the actual dagql query inside a buildkit operation, which
// allows for efficiently caching the result.
func NewDirectoryDagOp(
	ctx context.Context,
	srv *dagql.Server,
	dagop *FSDagOp,
	inputs []llb.State,
) (*Directory, error) {
	st, err := newFSDagOp[*Directory](ctx, dagop, inputs)
	if err != nil {
		return nil, err
	}
	query, ok := srv.Root().(dagql.Instance[*Query])
	if !ok {
		return nil, fmt.Errorf("server root was %T", srv.Root())
	}
	return NewDirectorySt(ctx, query.Self, st, dagop.Path, query.Self.Platform(), nil)
}

// NewFileDagOp takes a target ID for a File, and returns a File for it,
// computing the actual dagql query inside a buildkit operation, which allows
// for efficiently caching the result.
func NewFileDagOp(
	ctx context.Context,
	srv *dagql.Server,
	dagop *FSDagOp,
	inputs []llb.State,
) (*File, error) {
	st, err := newFSDagOp[*File](ctx, dagop, inputs)
	if err != nil {
		return nil, err
	}
	query, ok := srv.Root().(dagql.Instance[*Query])
	if !ok {
		return nil, fmt.Errorf("server root was %T", srv.Root())
	}
	return NewFileSt(ctx, query.Self, st, dagop.Path, query.Self.Platform(), nil)
}

func newFSDagOp[T dagql.Typed](
	ctx context.Context,
	dagop *FSDagOp,
	inputs []llb.State,
) (llb.State, error) {
	if dagop.ID == nil {
		return llb.State{}, fmt.Errorf("dagop ID is nil")
	}

	var t T
	requiredType := t.Type().NamedType
	if dagop.ID.Type().NamedType() != requiredType {
		return llb.State{}, fmt.Errorf("expected %s to be selected, instead got %s", requiredType, dagop.ID.Type().NamedType())
	}

	return newDagOpLLB(ctx, dagop, dagop.ID, inputs)
}

type FSDagOp struct {
	ID *call.ID

	// Path is the target path for the output - this is mostly ignored by dagop
	// (except for contributing to the cache key). However, it can be used by
	// dagql running inside a dagop to determine where it should write data.
	Path string

	// Data is any additional data that should be passed to the dagop. It does
	// not contribute to the cache key.
	Data any

	// utility values set in the context of an Exec
	g   bksession.Group
	opt buildkit.OpOpts
}

func (op FSDagOp) Name() string {
	return "dagop.fs"
}

func (op FSDagOp) Backend() buildkit.CustomOpBackend {
	return &op
}

func (op FSDagOp) Digest() (digest.Digest, error) {
	opData, err := json.Marshal(op.Data)
	if err != nil {
		return "", err
	}
	return digest.FromString(strings.Join([]string{
		op.ID.Digest().String(),
		op.Path,
		string(opData),
	}, "+")), nil
}
func (op FSDagOp) CacheKey(ctx context.Context) (key digest.Digest, err error) {
	return digest.FromString(strings.Join([]string{
		op.ID.Digest().String(),
		op.Path,
	}, "+")), nil
}

func (op FSDagOp) Exec(ctx context.Context, g bksession.Group, inputs []solver.Result, opt buildkit.OpOpts) (outputs []solver.Result, err error) {
	op.g = g
	op.opt = opt
	obj, err := opt.Server.Load(withDagOpContext(ctx, op), op.ID)
	if err != nil {
		return nil, err
	}

	switch inst := obj.(type) {
	case dagql.Instance[*Directory]:
		if inst.Self.Result != nil {
			ref := worker.NewWorkerRefResult(inst.Self.Result.Clone(), opt.Worker)
			return []solver.Result{ref}, nil
		}

		res, err := inst.Self.Evaluate(ctx)
		if err != nil {
			return nil, err
		}
		ref, err := res.Ref.Result(ctx)
		if err != nil {
			return nil, err
		}
		return []solver.Result{ref}, nil

	case dagql.Instance[*File]:
		if inst.Self.Result != nil {
			ref := worker.NewWorkerRefResult(inst.Self.Result.Clone(), opt.Worker)
			return []solver.Result{ref}, nil
		}

		res, err := inst.Self.Evaluate(ctx)
		if err != nil {
			return nil, err
		}
		ref, err := res.Ref.Result(ctx)
		if err != nil {
			return nil, err
		}
		return []solver.Result{ref}, nil

	default:
		// shouldn't happen, should have errored in DagLLB already
		return nil, fmt.Errorf("expected FS to be selected, instead got %T", obj)
	}
}

func (op FSDagOp) Group() bksession.Group {
	return op.g
}

func (op FSDagOp) Mount(ctx context.Context, ref bkcache.Ref, f func(string) error) error {
	return MountRef(ctx, ref, op.g, f)
}

// NewRawDagOp takes a target ID for any JSON-serializable dagql type, and returns
// it, computing the actual dagql query inside a buildkit operation, which
// allows for efficiently caching the result.
func NewRawDagOp[T dagql.Typed](
	ctx context.Context,
	srv *dagql.Server,
	dagop *RawDagOp,
	inputs []llb.State,
) (t T, err error) {
	if dagop.ID == nil {
		return t, fmt.Errorf("dagop ID is nil")
	}
	if dagop.Filename == "" {
		return t, fmt.Errorf("dagop filename is empty")
	}

	st, err := newDagOpLLB(ctx, dagop, dagop.ID, inputs)
	if err != nil {
		return t, err
	}

	query, ok := srv.Root().(dagql.Instance[*Query])
	if !ok {
		return t, fmt.Errorf("server root was %T", srv.Root())
	}

	f, err := NewFileSt(ctx, query.Self, st, dagop.Filename, Platform{}, nil)
	if err != nil {
		return t, err
	}
	dt, err := f.Contents(ctx)
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(dt, &t)
	return t, err
}

type RawDagOp struct {
	ID       *call.ID
	Filename string
}

func (op RawDagOp) Name() string {
	return "dagop.raw"
}

func (op RawDagOp) Backend() buildkit.CustomOpBackend {
	return &op
}

func (op RawDagOp) Digest() (digest.Digest, error) {
	return digest.FromString(strings.Join([]string{
		op.ID.Digest().String(),
		op.Filename,
	}, "+")), nil
}

func (op RawDagOp) CacheKey(ctx context.Context) (key digest.Digest, err error) {
	return digest.FromString(strings.Join([]string{
		op.ID.Digest().String(),
		op.Filename,
	}, "+")), nil
}

func (op RawDagOp) Exec(ctx context.Context, g bksession.Group, inputs []solver.Result, opt buildkit.OpOpts) (outputs []solver.Result, retErr error) {
	result, err := opt.Server.LoadType(withDagOpContext(ctx, op), op.ID)
	if err != nil {
		return nil, err
	}
	if wrapped, ok := result.(dagql.Wrapper); ok {
		result = wrapped.Unwrap()
	}

	query, ok := opt.Server.Root().(dagql.Instance[*Query])
	if !ok {
		return nil, fmt.Errorf("server root was %T", opt.Server.Root())
	}
	ref, err := query.Self.BuildkitCache().New(ctx, nil, g,
		bkcache.CachePolicyRetain,
		bkcache.WithRecordType(client.UsageRecordTypeRegular),
		bkcache.WithDescription(op.Name()))
	if err != nil {
		return nil, fmt.Errorf("failed to create new mutable: %w", err)
	}
	defer func() {
		if retErr != nil && ref != nil {
			ref.Release(context.WithoutCancel(ctx))
		}
	}()

	mount, err := ref.Mount(ctx, false, g)
	if err != nil {
		return nil, err
	}
	lm := snapshot.LocalMounter(mount)
	dir, err := lm.Mount()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	f, err := os.Create(filepath.Join(dir, op.Filename))
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && f != nil {
			f.Close()
		}
	}()

	enc := json.NewEncoder(f)
	err = enc.Encode(result)
	if err != nil {
		return nil, err
	}
	err = f.Close()
	if err != nil {
		return nil, err
	}
	f = nil

	lm.Unmount()
	lm = nil

	snap, err := ref.Commit(ctx)
	if err != nil {
		return nil, err
	}
	ref = nil

	return []solver.Result{worker.NewWorkerRefResult(snap, opt.Worker)}, nil
}

func newDagOpLLB(ctx context.Context, dagOp buildkit.CustomOp, id *call.ID, inputs []llb.State) (llb.State, error) {
	return buildkit.NewCustomLLB(ctx, dagOp, inputs,
		llb.WithCustomNamef("%s %s", dagOp.Name(), id.Name()),
		buildkit.WithTracePropagation(ctx),
		buildkit.WithPassthrough(),
	)
}

type dagOpContextKey string

func withDagOpContext(ctx context.Context, op buildkit.CustomOp) context.Context {
	return context.WithValue(ctx, dagOpContextKey(op.Name()), op)
}

func DagOpFromContext[T buildkit.CustomOp](ctx context.Context) (t T, ok bool) {
	if val := ctx.Value(dagOpContextKey(t.Name())); val != nil {
		t, ok = val.(T)
	}
	return t, ok
}

func DagOpInContext[T buildkit.CustomOp](ctx context.Context) bool {
	_, ok := DagOpFromContext[T](ctx)
	return ok
}

// MountRef is a utility for easily mounting a ref
func MountRef(ctx context.Context, ref bkcache.Ref, g bksession.Group, f func(string) error) error {
	mount, err := ref.Mount(ctx, false, g)
	if err != nil {
		return err
	}
	lm := snapshot.LocalMounter(mount)
	defer lm.Unmount()

	dir, err := lm.Mount()
	if err != nil {
		return err
	}
	return f(dir)
}

func NewContainerDagOp(
	ctx context.Context,
	id *call.ID,
	ctr *Container,
) (*Container, error) {
	dagop := &ContainerDagOp{
		ID: id,
	}
	inputs := make([]llb.State, 0, len(ctr.Mounts))

	addMount := func(mnt ContainerMount) error {
		st, err := defToState(mnt.Source)
		if err != nil {
			return err
		}
		inputs = append(inputs, st)

		mount := &pb.Mount{
			Dest:     mnt.Target,
			Selector: mnt.SourcePath,
			Output:   pb.OutputIndex(len(dagop.Mounts)),
			Readonly: mnt.Readonly,
			// MountType: nil,
			// TmpfsOpt: nil,
			// CacheOpt: nil,
			// SecretOpt: nil,
			// SSHOpt: nil,
			// ContentCache: nil,
		}
		if st.Output() == nil {
			mount.Input = pb.Empty
		} else {
			mount.Input = pb.InputIndex(len(dagop.Mounts))
		}
		dagop.Mounts = append(dagop.Mounts, mount)

		return nil
	}

	if err := addMount(ContainerMount{Source: ctr.FS, Target: "/"}); err != nil {
		return nil, err
	}
	if err := addMount(ContainerMount{Source: ctr.Meta, Target: buildkit.MetaMountDestPath, SourcePath: buildkit.MetaMountDestPath}); err != nil {
		return nil, err
	}
	for _, mount := range ctr.Mounts {
		if err := addMount(mount); err != nil {
			return nil, err
		}
	}

	st, err := newContainerDagOp(ctx, dagop, inputs)
	if err != nil {
		return nil, err
	}

	ctr = ctr.Clone()

	def, err := st.Marshal(ctx, llb.Platform(ctr.Platform.Spec()))
	if err != nil {
		return nil, err
	}
	ctr.FS = def.ToPB()

	st = buildkit.StateIdx(st, 1)
	def, err = st.Marshal(ctx, llb.Platform(ctr.Platform.Spec()))
	if err != nil {
		return nil, err
	}
	ctr.Meta = def.ToPB()

	for i, mount := range ctr.Mounts {
		st := buildkit.StateIdx(st, i+2)
		def, err := st.Marshal(ctx, llb.Platform(ctr.Platform.Spec()))
		if err != nil {
			return nil, err
		}
		mount.Source = def.ToPB()
		ctr.Mounts[i] = mount
	}

	return ctr, nil
}

func newContainerDagOp(
	ctx context.Context,
	dagop *ContainerDagOp,
	inputs []llb.State,
) (llb.State, error) {
	if dagop.ID == nil {
		return llb.State{}, fmt.Errorf("dagop ID is nil")
	}
	if len(inputs) != len(dagop.Mounts) {
		return llb.State{}, fmt.Errorf("mount count did not match %d != %d", len(inputs), len(dagop.Mounts))
	}

	var t Container
	requiredType := t.Type().NamedType
	if dagop.ID.Type().NamedType() != requiredType {
		return llb.State{}, fmt.Errorf("expected %s to be selected, instead got %s", requiredType, dagop.ID.Type().NamedType())
	}

	return newDagOpLLB(ctx, dagop, dagop.ID, inputs)
}

type ContainerDagOp struct {
	ID *call.ID

	// XXX: this currently only includes destinations
	// the other info is also relevant :eyes:
	Mounts []*pb.Mount

	// Data is any additional data that should be passed to the dagop. It does
	// not contribute to the cache key.
	Data any

	// utility values set in the context of an Exec
	g   bksession.Group
	opt buildkit.OpOpts

	inputs []solver.Result
}

func (op ContainerDagOp) GetMounts() []bkcache.ImmutableRef {
	refs := make([]bkcache.ImmutableRef, 0, len(op.inputs))
	for _, input := range op.inputs {
		refs = append(refs, input.Sys().(*worker.WorkerRef).ImmutableRef)
	}
	return refs
}

func (op ContainerDagOp) Root() *pb.Mount {
	return op.Mounts[0]
}

func (op ContainerDagOp) Meta() *pb.Mount {
	return op.Mounts[1]
}

func (op ContainerDagOp) Other() []*pb.Mount {
	return op.Mounts[2:]
}

func (op ContainerDagOp) GetInput(mount *pb.Mount) bkcache.ImmutableRef {
	if mount.Input == pb.Empty {
		return nil
	}
	input := op.inputs[mount.Input]
	return input.Sys().(*worker.WorkerRef).ImmutableRef
}

func (op ContainerDagOp) Name() string {
	return "dagop.ctr"
}

func (op ContainerDagOp) Backend() buildkit.CustomOpBackend {
	return &op
}

func (op ContainerDagOp) Group() bksession.Group {
	return op.g
}

func (op ContainerDagOp) Digest() (digest.Digest, error) {
	opData, err := json.Marshal(op.Data)
	if err != nil {
		return "", err
	}
	mountsData, err := json.Marshal(op.Mounts)
	if err != nil {
		return "", err
	}
	return digest.FromString(strings.Join([]string{
		op.ID.Digest().String(),
		string(mountsData),
		string(opData),
	}, "+")), nil
}

func (op ContainerDagOp) CacheKey(ctx context.Context) (key digest.Digest, err error) {
	// XXX: will need proper cache map control here
	return op.Digest()
}

func (op ContainerDagOp) Exec(ctx context.Context, g bksession.Group, inputs []solver.Result, opt buildkit.OpOpts) (outputs []solver.Result, retErr error) {
	// if len(inputs) != len(op.Mounts) {
	// 	return nil, fmt.Errorf("input count did not match %d != %d", len(inputs), len(op.Mounts))
	// }

	op.g = g
	op.opt = opt
	op.inputs = inputs

	// XXX: can we somehow populate result *before* call? that would be a nice easy way to get the inputs
	obj, err := opt.Server.Load(withDagOpContext(ctx, op), op.ID)
	if err != nil {
		return nil, err
	}

	switch inst := obj.(type) {
	case dagql.Instance[*Container]:
		// TODO: properly handle outputs
		refs := make([]solver.Result, 0, 2+len(inst.Self.Mounts))
		if inst.Self.FSResult != nil {
			ref := worker.NewWorkerRefResult(inst.Self.FSResult.Clone(), opt.Worker)
			refs = append(refs, ref)
		} else {
			res, err := inst.Self.Evaluate(ctx)
			if err != nil {
				return nil, err
			}
			ref, err := res.Ref.Result(ctx)
			if err != nil {
				return nil, err
			}
			refs = append(refs, ref)
		}

		bk, err := inst.Self.Query.Buildkit(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get buildkit client: %w", err)
		}

		if inst.Self.MetaResult != nil {
			ref := worker.NewWorkerRefResult(inst.Self.MetaResult.Clone(), opt.Worker)
			refs = append(refs, ref)
		} else {
			res, err := bk.Solve(ctx, bkgw.SolveRequest{
				Evaluate:   true,
				Definition: inst.Self.Meta,
			})
			if err != nil {
				return nil, err
			}
			ref, err := res.Ref.Result(ctx)
			if err != nil {
				return nil, err
			}
			refs = append(refs, ref)
		}

		// XXX: a lot of this can go into core/container.go
		for _, mount := range inst.Self.Mounts {
			if mount.Result != nil {
				ref := worker.NewWorkerRefResult(mount.Result.Clone(), opt.Worker)
				refs = append(refs, ref)
			} else {
				res, err := bk.Solve(ctx, bkgw.SolveRequest{
					Evaluate:   true,
					Definition: mount.Source,
				})
				if err != nil {
					return nil, err
				}
				ref, err := res.Ref.Result(ctx)
				if err != nil {
					return nil, err
				}
				refs = append(refs, ref)
			}
		}

		fmt.Println("refs", refs)
		return refs, nil

	default:
		// shouldn't happen, should have errored in DagLLB already
		return nil, fmt.Errorf("expected FS to be selected, instead got %T", obj)
	}
}
