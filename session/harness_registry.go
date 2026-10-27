package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/internal/ctxlock"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// PlacerFactory builds the Placer for a registered harness, and a release func that frees whatever
// it built (nil if nothing). It is where the host decides which placements it can serve and checks
// a placement's address forms: an error for a new registration is FailedPrecondition and nothing
// is stored. At startup, an error marks only that stored registration unservable. The factory must
// not dial or provision anything. Explicit model-policy configuration adds a bounded Describe
// admission after construction: registration/load fails closed if that check fails. Unconfigured
// harnesses keep the existing no-dial registration behavior.
//
// The Placer must be built with placement.WithDescriptorID(spec.GetDescriptorId()), so that every
// Exec, Resume and Fork refuses a harness that reports another descriptor id. HarnessRegistry
// checks this and refuses a Placer that does not expect exactly that id, so a factory that drops the
// option fails closed instead of running turns on whatever answers.
//
// The Placer must be new: not used, and not in any Registry. placement.Registry.Add wires it to the
// registry's shared session guard and checkpoint state, so its sessions cannot overlap calls
// through a static harness, and refuses a Placer that is in use or in another Registry.
//
// For a RemotePlacement, runtime/remote is the backend that dials a harness already running at an
// address; agentsessionsd's -harness flag uses it the same way.
type PlacerFactory func(name string, spec *v1.HarnessSpec) (p *placement.Placer, release func(), err error)

// HarnessRegistry implements v1.HarnessRegistryServer over the store and placement registry the
// Sessions service routes through.
//
// It is an admin surface: whoever can call RegisterHarness decides where the host sends session
// history and whose credential pays for model calls. agentsessionsd serves it only on an optional
// separate literal-loopback listener; neither default service authenticates callers. Embedders
// can opt into WithRegistryAuthorization and authn interceptors, but must still keep the service
// off the Sessions listener and protect its reachability and transport.
type HarnessRegistry struct {
	v1.UnimplementedHarnessRegistryServer
	store    HarnessStore
	registry *placement.Registry
	factory  PlacerFactory
	auth     *authorization
	logger   *slog.Logger

	// mu serializes RegisterHarness, so building a Placer, storing the row and adding the Placer
	// happen as one step per name within this process. Waiting must honor caller cancellation:
	// policy admission may hold this gate across a remote Describe. Close also takes the gate.
	mu       ctxlock.Mutex
	releases []func()
}

// WithRegistryLogger routes registry operational logs, including enforced-mode
// diagnostics and startup warnings, to logger. A nil logger uses slog.Default().
// It is independent of WithLogger, which configures only the Sessions service.
func WithRegistryLogger(logger *slog.Logger) HarnessRegistryOption {
	return func(h *HarnessRegistry) { h.logger = logger }
}

// NewHarnessRegistry builds the service, retaining every stored name (including retired ones)
// and resolving each usable registration to a Placer. Existing sessions on a retired harness still
// run. Per-registration spec, factory and descriptor failures leave only that name unservable;
// store enumeration/reservation errors and static-name collisions still refuse startup. It first
// reserves the registry's static and reserved names in the store, as ReserveStaticHarnessNames does,
// rather than letting one name mean two harnesses.
//
// Operational logs use slog.Default() unless WithRegistryLogger is supplied.
// Registrations are loaded at startup. A registration another process makes in a shared database
// is served after this host restarts, or if RegisterHarness loads its identical compatible spec.
func NewHarnessRegistry(store HarnessStore, registry *placement.Registry, factory PlacerFactory, opts ...HarnessRegistryOption) (*HarnessRegistry, error) {
	if nilStore(store) || registry == nil || factory == nil {
		return nil, errors.New("session: NewHarnessRegistry needs a store, a registry and a factory")
	}
	h := &HarnessRegistry{store: store, registry: registry, factory: factory}
	for _, opt := range opts {
		opt(h)
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	ctx := observability.EnsureRequestID(context.Background())
	if err := ReserveStaticHarnessNames(store, registry); err != nil {
		logHarnessRegistryStoreError(ctx, h.logger, "NewHarnessRegistry", err)
		return nil, err
	}
	if err := forEachRegistration(ctx, h.logger, store, func(rec sqlitelog.HarnessRecord) error { return h.load(ctx, rec) }); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// ReserveStaticHarnessNames reserves, in the store, every name the registry holds for a static
// harness, so that no host sharing the store can register one of them, now or later. It fails if
// one of them already has a registration, active or retired. Every host that routes sessions
// through a store other hosts may register harnesses in calls it before serving: otherwise a
// registration made by a host whose static names differ would mean a second harness under one of
// this host's names. NewHarnessRegistry calls it.
//
// Service also refuses, when it creates or routes a session, a static name that has a
// registration row, so a host that skips this call fails closed instead of mixing the two. The
// reservation is what lets the registering host refuse the name up front instead.
func ReserveStaticHarnessNames(store HarnessStore, registry *placement.Registry) error {
	if nilStore(store) || registry == nil {
		return errors.New("session: ReserveStaticHarnessNames needs a store and a registry")
	}
	if err := store.ReserveHarnessNames(registry.ReservedNames()); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// forEachRegistration calls fn for every stored registration, retired ones included, in name order.
func forEachRegistration(ctx context.Context, logger *slog.Logger, store HarnessStore, fn func(sqlitelog.HarnessRecord) error) error {
	after := ""
	for {
		recs, err := store.ListHarnesses(sqlitelog.HarnessListOptions{After: after, Limit: sqlitelog.MaxPageSize, IncludeRetired: true})
		if err != nil {
			logHarnessRegistryStoreError(ctx, logger, "NewHarnessRegistry", err)
			return err
		}
		for _, rec := range recs {
			if err := fn(rec); err != nil {
				return err
			}
		}
		if len(recs) < sqlitelog.MaxPageSize {
			return nil
		}
		after = recs[len(recs)-1].Name
	}
}

func (h *HarnessRegistry) load(ctx context.Context, rec sqlitelog.HarnessRecord) error {
	spec, err := storedSpec(rec.Spec)
	if err != nil {
		return h.unavailable(ctx, rec.Name, err)
	}
	p, release, err := h.factory(rec.Name, spec)
	if err == nil {
		err = checkPlacer(ctx, p, spec)
	}
	if err != nil {
		if release != nil {
			release()
		}
		return h.unavailable(ctx, rec.Name, err)
	}
	if err := h.registry.Add(rec.Name, p); err != nil {
		if release != nil {
			release()
		}
		if errors.Is(err, placement.ErrHarnessExists) {
			return err
		}
		return h.unavailable(ctx, rec.Name, err)
	}
	h.keep(release)
	return nil
}

func (h *HarnessRegistry) unavailable(ctx context.Context, name string, cause error) error {
	if err := h.registry.MarkUnavailable(name, cause); err != nil {
		return err
	}
	// Factory/runtime errors may carry peer payloads. A wrapped storage sentinel does not
	// establish trust; only direct store calls below retain a full cause in operator logs.
	observability.Logger(ctx, h.logger).WarnContext(ctx, "stored harness cannot be served on this host",
		"harness", name, "method", "NewHarnessRegistry", "error_kind", "harness_unservable", "error_category", "external")
	return nil
}

func logHarnessRegistryStoreError(ctx context.Context, logger *slog.Logger, method string, err error) {
	observability.Logger(ctx, logger).ErrorContext(ctx, "harness registry store operation failed",
		"method", method, "error_kind", "internal", "error_category", "host", "error", err)
}

func logHarnessRegistryExternalError(ctx context.Context, logger *slog.Logger, method string, err error) {
	observability.Logger(ctx, logger).ErrorContext(ctx, "harness registration admission failed",
		"method", method,
		"error_kind", serviceErrorKind(err), "error_category", "external")
}

// checkPlacer refuses what a PlacerFactory returned when it cannot serve the spec as registered: no
// Placer at all, one that does not check the descriptor id the spec expects, or an invalid explicit
// model policy. Policy admission is cached before the immutable row commits, so Registry.Add
// cannot perform a second Describe with a different result after commit.
func checkPlacer(ctx context.Context, p *placement.Placer, spec *v1.HarnessSpec) error {
	if p == nil {
		return errors.New("the placer factory returned no placer")
	}
	if got, want := p.DescriptorID(), spec.GetDescriptorId(); got != want {
		return fmt.Errorf("the placer factory returned a placer that expects descriptor id %q, the spec expects %q; build it with placement.WithDescriptorID(spec.GetDescriptorId())", got, want)
	}
	return p.ValidateModelPolicy(ctx)
}

func (h *HarnessRegistry) keep(release func()) {
	if release != nil {
		h.releases = append(h.releases, release)
	}
}

// Close releases every Placer the factory built. Call it after the servers that route through the
// registry have stopped.
func (h *HarnessRegistry) Close() {
	_ = h.mu.Lock(context.Background()) // Close must drain the active registration before release.
	defer h.mu.Unlock()
	for _, release := range h.releases {
		release()
	}
	h.releases = nil
}

// harnessName is an RFC 1123 DNS label.
var harnessName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Bounds on what one registration stores. They are far above any real spec or reason, and exist so
// an admin call cannot grow the database without limit.
const (
	maxSpecBytes     = 4096
	maxRetireReason  = 1024
	observeTimeout   = 10 * time.Second
	harnessUIDPrefix = "harness-"
)

// specDigest returns the spec's canonical bytes and "sha256:<hex>" over them. The canonical form is
// the journal's (package canon), so any implementation can recompute the digest.
func specDigest(spec *v1.HarnessSpec) (canonical []byte, digest string, err error) {
	c, err := canon.Proto(spec)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(c)
	return c, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func decodeSpec(s string) (*v1.HarnessSpec, error) {
	spec := &v1.HarnessSpec{}
	if err := protojson.Unmarshal([]byte(s), spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// storedSpec never returns a partially decoded or unsupported stored spec. Unknown fields and enum
// values can be written by a newer host sharing this journal and must fail closed on this one.
func storedSpec(s string) (*v1.HarnessSpec, error) {
	spec, err := decodeSpec(s)
	if err == nil {
		err = validateSpec(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("stored spec cannot be decoded or validated (possible host/schema version skew; upgrade this host or restart with a compatible version): %w", err)
	}
	return spec, nil
}

func validateSpec(spec *v1.HarnessSpec) error {
	if spec == nil {
		return errors.New("spec is required")
	}
	// A field this build does not know would be dropped from the canonical form, so the digest and
	// the stored spec would not say what the caller asked for. An enum number this build does not
	// define is kept, but it declares something, such as a resumability, this host cannot honor.
	if err := checkKnown(spec.ProtoReflect()); err != nil {
		return err
	}
	switch p := spec.GetPlacement().(type) {
	case *v1.HarnessSpec_Remote:
		if p.Remote.GetAddress() == "" {
			return errors.New("remote.address is required")
		}
	case *v1.HarnessSpec_Substrate:
		if p.Substrate.GetAtespace() == "" || p.Substrate.GetTemplate() == "" {
			return errors.New("substrate.atespace and substrate.template are required")
		}
	default:
		return errors.New("placement is required")
	}
	if spec.GetCapabilities().GetResumability() == v1.Resumability_RESUMABILITY_UNSPECIFIED {
		return errors.New("capabilities.resumability is required")
	}
	return nil
}

// checkKnown reports the first unknown field, or enum value this build does not define, in m or in
// any message it holds.
func checkKnown(m protoreflect.Message) error {
	if len(m.GetUnknown()) > 0 {
		return fmt.Errorf("%s has fields this host does not understand", m.Descriptor().Name())
	}
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			// HarnessSpec has no map fields; refuse one rather than skip what it holds.
			err = fmt.Errorf("%s: map fields are not supported", fd.Name())
		case fd.IsList():
			for i := 0; i < v.List().Len() && err == nil; i++ {
				err = checkValue(fd, v.List().Get(i))
			}
		default:
			err = checkValue(fd, v)
		}
		return err == nil
	})
	return err
}

func checkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch {
	case fd.Message() != nil:
		return checkKnown(v.Message())
	case fd.Enum() != nil:
		if fd.Enum().Values().ByNumber(v.Enum()) == nil {
			return fmt.Errorf("%s: %d is not a value this host understands", fd.Name(), v.Enum())
		}
	}
	return nil
}

func newHarnessUID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return harnessUIDPrefix + hex.EncodeToString(b[:])
}

func validName(name string) error {
	if !harnessName.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "name %q is not a DNS label (lowercase alphanumerics and '-', at most 63)", name)
	}
	return nil
}

// RegisterHarness stores a registration and makes it resolvable, or recognizes a repeat of one.
func (h *HarnessRegistry) RegisterHarness(ctx context.Context, req *v1.RegisterHarnessRequest) (*v1.RegisterHarnessResponse, error) {
	return h.register(ctx, v1.HarnessRegistry_RegisterHarness_FullMethodName, req.GetName(), req.GetSpec(), false)
}

// CreateHarness stores a new registration and makes it resolvable, only if the name was never
// registered. Unlike RegisterHarness, it refuses an identical spec too, and never reactivates or
// loads an existing row. The store's transaction, not this host's lock, is what holds against
// another host sharing the journal.
func (h *HarnessRegistry) CreateHarness(ctx context.Context, req *v1.CreateHarnessRequest) (*v1.HarnessRegistration, error) {
	resp, err := h.register(ctx, v1.HarnessRegistry_CreateHarness_FullMethodName, req.GetName(), req.GetSpec(), true)
	if err != nil {
		return nil, err
	}
	return resp.GetHarness(), nil
}

// register is RegisterHarness, or CreateHarness when createOnly is set. method is the RPC's full
// name, for authorization and diagnostics; both need the registry.register action.
func (h *HarnessRegistry) register(ctx context.Context, method, name string, spec *v1.HarnessSpec, createOnly bool) (_ *v1.RegisterHarnessResponse, err error) {
	ctx = observability.EnsureRequestID(ctx)
	defer h.auth.internalError(ctx, h.logger, method, &err)
	if err := h.auth.global(ctx, method, api.ActionRegistryRegister); err != nil {
		return nil, err
	}
	if err := validName(name); err != nil {
		return nil, err
	}
	if h.registry.Reserved(name) {
		return nil, status.Errorf(codes.AlreadyExists, "harness %q is a static harness name on this host and cannot be registered", name)
	}
	if err := validateSpec(spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec: %v", err)
	}
	canonical, digest, err := specDigest(spec)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec: %v", err)
	}
	if len(canonical) > maxSpecBytes {
		return nil, status.Errorf(codes.InvalidArgument, "spec is %d bytes in canonical form; the limit is %d", len(canonical), maxSpecBytes)
	}

	if err := h.mu.Lock(ctx); err != nil {
		return nil, registrationInterrupted(err)
	}
	defer h.mu.Unlock()
	// Lock may succeed with a cancelled context. An expired caller must not build or store anything,
	// including a route without model policy (which does not inspect the context itself).
	if err := ctx.Err(); err != nil {
		return nil, registrationInterrupted(err)
	}
	// Fail a taken name or a conflicting spec before building anything for it. The store checks
	// again under its write lock, which is what holds against another process.
	if rec, err := h.store.Harness(name); err == nil && createOnly {
		return nil, status.Errorf(codes.AlreadyExists, "harness %q is already registered (%s, spec %s); create-only registration needs a name that was never registered", name, rec.State, rec.SpecDigest)
	} else if err == nil && rec.SpecDigest != digest {
		return nil, status.Errorf(codes.AlreadyExists, "harness %q holds spec %s, request is %s; registrations are immutable, use a new name", name, rec.SpecDigest, digest)
	} else if err != nil && !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
		// Enforced INTERNAL errors are logged once by the deferred authorization boundary.
		if h.auth == nil {
			logHarnessRegistryStoreError(ctx, h.logger, method, err)
		}
		return nil, hostInternalError("register: %v", err)
	}
	// A name that a host sharing the journal reserved, and this host's registry does not hold, is
	// taken too. CreateHarness refuses it ahead of placement admission (REG-3a), so a placement this
	// host cannot build does not turn ALREADY_EXISTS into FAILED_PRECONDITION. The store checks
	// again in the transaction that would insert the row.
	if createOnly {
		if reserved, err := h.store.HarnessNameReserved(name); err != nil {
			if h.auth == nil {
				logHarnessRegistryStoreError(ctx, h.logger, method, err)
			}
			return nil, hostInternalError("register: %v", err)
		} else if reserved {
			return nil, status.Errorf(codes.AlreadyExists, "%v: %q by a host sharing this journal and cannot be registered", sqlitelog.ErrHarnessNameReserved, name)
		}
	}
	// Build the Placer before anything is stored, so a placement this host cannot serve is refused
	// with nothing written. It is added only after the row commits: a Placer resolvable under a
	// name whose row then failed to store would run sessions on a spec nobody registered.
	var placer *placement.Placer
	var release func()
	if !h.registry.Has(name) {
		if placer, release, err = h.factory(name, spec); err != nil {
			if release != nil {
				release()
			}
			err = status.Errorf(codes.FailedPrecondition, "harness %q: %v", name, err)
			logHarnessRegistryExternalError(ctx, h.logger, method, err)
			return nil, err
		}
		if err := checkPlacer(ctx, placer, spec); err != nil {
			// Storing the row would report CREATED for a name no session can use, or one whose
			// turns skip the descriptor check, and the row is immutable.
			if release != nil {
				release()
			}
			code := codes.Internal // nil Placer or a dropped descriptor-ID check is a host bug.
			switch {
			case errors.Is(err, api.ErrModelNotAllowed), errors.Is(err, api.ErrInvalidModelPolicy):
				code = codes.FailedPrecondition
			case errors.Is(err, placement.ErrHarnessUnavailable):
				code = codes.Unavailable
			case errors.Is(err, placement.ErrAdmissionInterrupted):
				code = admissionInterruptedCode(err)
			}
			err = withStatusCause(code, fmt.Errorf("harness %q: %w", name, err))
			if h.auth == nil || code != codes.Internal {
				logHarnessRegistryExternalError(ctx, h.logger, method, err)
			}
			return nil, err
		}
	}
	// Describe may return success despite cancellation. Fence the immutable write as well as
	// admission to serialization, and release any uncommitted factory resources.
	if err := ctx.Err(); err != nil {
		if release != nil {
			release()
		}
		return nil, registrationInterrupted(err)
	}
	want := sqlitelog.HarnessRecord{Name: name, UID: newHarnessUID(), Spec: string(canonical), SpecDigest: digest}
	var rec sqlitelog.HarnessRecord
	result := sqlitelog.HarnessCreated // CreateHarness either creates the row or fails
	if createOnly {
		rec, err = h.store.CreateHarness(want)
	} else {
		rec, result, err = h.store.RegisterHarness(want)
	}
	if err != nil {
		if release != nil {
			release()
		}
		if errors.Is(err, sqlitelog.ErrHarnessExists) {
			return nil, status.Errorf(codes.AlreadyExists, "%v; create-only registration needs a name that was never registered", err)
		}
		if errors.Is(err, sqlitelog.ErrHarnessSpecConflict) {
			return nil, status.Errorf(codes.AlreadyExists, "%v; registrations are immutable, use a new name", err)
		}
		if errors.Is(err, sqlitelog.ErrHarnessNameReserved) {
			return nil, status.Errorf(codes.AlreadyExists, "%v by a host sharing this journal and cannot be registered", err)
		}
		if h.auth == nil {
			logHarnessRegistryStoreError(ctx, h.logger, method, err)
		}
		return nil, hostInternalError("register: %v", err)
	}
	if placer != nil {
		if err := h.registry.Add(name, placer); err != nil {
			if release != nil {
				release()
			}
			return nil, hostInternalError("register: %v", err)
		}
		h.keep(release)
	}
	return &v1.RegisterHarnessResponse{Harness: h.registrationProto(rec), Outcome: registerOutcome(result)}, nil
}

func registrationInterrupted(cause error) error {
	return withStatusCause(admissionInterruptedCode(cause),
		fmt.Errorf("%w: register: %w", placement.ErrAdmissionInterrupted, cause))
}

func registerOutcome(r sqlitelog.RegisterResult) v1.RegisterOutcome {
	switch r {
	case sqlitelog.HarnessCreated:
		return v1.RegisterOutcome_REGISTER_OUTCOME_CREATED
	case sqlitelog.HarnessUnchanged:
		return v1.RegisterOutcome_REGISTER_OUTCOME_UNCHANGED
	case sqlitelog.HarnessReactivated:
		return v1.RegisterOutcome_REGISTER_OUTCOME_REACTIVATED
	}
	return v1.RegisterOutcome_REGISTER_OUTCOME_UNSPECIFIED
}

// GetHarness returns one static or registered harness, optionally with what it reports right now.
func (h *HarnessRegistry) GetHarness(ctx context.Context, req *v1.GetHarnessRequest) (_ *v1.HarnessRegistration, err error) {
	ctx = observability.EnsureRequestID(ctx)
	defer h.auth.internalError(ctx, h.logger, v1.HarnessRegistry_GetHarness_FullMethodName, &err)
	if err := h.auth.global(ctx, v1.HarnessRegistry_GetHarness_FullMethodName, api.ActionRegistryRead); err != nil {
		return nil, err
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	var out *v1.HarnessRegistration
	if h.registry.Static(name) {
		out = staticProto(name)
	} else {
		rec, err := h.store.Harness(name)
		if errors.Is(err, sqlitelog.ErrHarnessNotFound) {
			return nil, status.Errorf(codes.NotFound, "harness %q not found", name)
		}
		if err != nil {
			return nil, hostInternalError("get: %v", err)
		}
		out = h.registrationProto(rec)
	}
	if req.GetObserve() {
		h.observe(ctx, out)
	}
	return out, nil
}

// observe fills observed or observe_error. A failed Describe is reported, not returned: the
// registration exists either way, and telling "registered but unreachable" from "not registered" is
// the point of asking.
func (h *HarnessRegistry) observe(ctx context.Context, out *v1.HarnessRegistration) {
	name := out.GetMetadata().GetName()
	p, err := h.registry.For(name)
	if err != nil {
		if errors.Is(err, placement.ErrUnknownHarness) {
			out.ObserveError = "harness is not loaded on this host"
		} else {
			out.ObserveError = err.Error()
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()
	desc, err := p.Describe(ctx)
	if err != nil {
		out.ObserveError = fmt.Sprintf("describe: %v", err)
		return
	}
	out.Observed = harnesswire.DescriptorToProto(desc)
}

// RetireHarness refuses new sessions on a registered harness. Existing sessions keep running.
func (h *HarnessRegistry) RetireHarness(ctx context.Context, req *v1.RetireHarnessRequest) (_ *v1.HarnessRegistration, err error) {
	ctx = observability.EnsureRequestID(ctx)
	defer h.auth.internalError(ctx, h.logger, v1.HarnessRegistry_RetireHarness_FullMethodName, &err)
	if err := h.auth.global(ctx, v1.HarnessRegistry_RetireHarness_FullMethodName, api.ActionRegistryRetire); err != nil {
		return nil, err
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if h.registry.Static(name) {
		return nil, status.Errorf(codes.FailedPrecondition, "harness %q is a static harness; it is retired by changing the host's configuration", name)
	}
	if len(req.GetReason()) > maxRetireReason {
		return nil, status.Errorf(codes.InvalidArgument, "reason is %d bytes; the limit is %d", len(req.GetReason()), maxRetireReason)
	}
	rec, err := h.store.RetireHarness(name, req.GetReason())
	if errors.Is(err, sqlitelog.ErrHarnessNotFound) {
		return nil, status.Errorf(codes.NotFound, "harness %q not found", name)
	}
	if err != nil {
		return nil, hostInternalError("retire: %v", err)
	}
	return h.registrationProto(rec), nil
}

// ListHarnesses lists static and registered harnesses together in name order. Paging is keyset on
// the name, so a registration made while a caller pages neither skips nor repeats an entry.
func (h *HarnessRegistry) ListHarnesses(ctx context.Context, req *v1.ListHarnessesRequest) (_ *v1.ListHarnessesResponse, err error) {
	ctx = observability.EnsureRequestID(ctx)
	defer h.auth.internalError(ctx, h.logger, v1.HarnessRegistry_ListHarnesses_FullMethodName, &err)
	if err := h.auth.global(ctx, v1.HarnessRegistry_ListHarnesses_FullMethodName, api.ActionRegistryList); err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	switch {
	case size < 0:
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	case size == 0:
		size = sqlitelog.DefaultPageSize
	case size > sqlitelog.MaxPageSize:
		size = sqlitelog.MaxPageSize
	}
	after, err := decodeHarnessPageToken(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "page_token: %v", err)
	}
	// One extra row says whether another page exists.
	recs, err := h.store.ListHarnesses(sqlitelog.HarnessListOptions{After: after, Limit: size + 1, IncludeRetired: req.GetIncludeRetired()})
	if err != nil {
		return nil, hostInternalError("list: %v", err)
	}
	var static []string
	for _, name := range h.registry.StaticNames() {
		if name > after {
			static = append(static, name)
		}
	}
	out := &v1.ListHarnessesResponse{}
	for len(out.Harnesses) <= size && (len(static) > 0 || len(recs) > 0) {
		if len(recs) == 0 || (len(static) > 0 && static[0] < recs[0].Name) {
			out.Harnesses = append(out.Harnesses, staticProto(static[0]))
			static = static[1:]
			continue
		}
		out.Harnesses = append(out.Harnesses, h.registrationProto(recs[0]))
		recs = recs[1:]
	}
	if len(out.Harnesses) > size {
		out.Harnesses = out.Harnesses[:size]
		out.NextPageToken = encodeHarnessPageToken(out.Harnesses[size-1].GetMetadata().GetName())
	}
	return out, nil
}

func encodeHarnessPageToken(after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(after))
}

func decodeHarnessPageToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) == 0 {
		return "", errors.New("not a token this server issued")
	}
	return string(b), nil
}

func staticProto(name string) *v1.HarnessRegistration {
	return &v1.HarnessRegistration{
		Metadata: &v1.ResourceMetadata{Name: name},
		State:    v1.HarnessState_HARNESS_STATE_ACTIVE,
		Source:   v1.HarnessSource_HARNESS_SOURCE_STATIC,
	}
}

func (h *HarnessRegistry) registrationProto(rec sqlitelog.HarnessRecord) *v1.HarnessRegistration {
	spec, specErr := storedSpec(rec.Spec)
	out := &v1.HarnessRegistration{
		Metadata: &v1.ResourceMetadata{
			Name:       rec.Name,
			Uid:        rec.UID,
			CreateTime: timestamppb.New(rec.CreatedAt),
			UpdateTime: timestamppb.New(rec.UpdatedAt),
		},
		Spec:       spec,
		SpecDigest: rec.SpecDigest,
		State:      v1.HarnessState_HARNESS_STATE_ACTIVE,
		Source:     v1.HarnessSource_HARNESS_SOURCE_REGISTERED,
	}
	if rec.State == sqlitelog.HarnessRetired {
		out.State = v1.HarnessState_HARNESS_STATE_RETIRED
		out.RetireTime = timestamppb.New(rec.RetiredAt)
		out.RetireReason = rec.RetireReason
	}
	if specErr != nil {
		out.UnservableReason = specErr.Error()
	} else if _, err := h.registry.For(rec.Name); err != nil {
		if errors.Is(err, placement.ErrHarnessUnservable) {
			out.UnservableReason = err.Error()
		} else if rec.State == sqlitelog.HarnessRetired {
			out.UnservableReason = "registration is not loaded on this host; restart this host to serve its existing sessions"
		} else {
			out.UnservableReason = "registration is not loaded on this host; restart this host or register the identical compatible spec locally to serve it"
		}
	}
	return out
}
