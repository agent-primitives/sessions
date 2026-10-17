package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) forkChildren(ctx context.Context, req *v1.ForkRequest, placer *placement.Placer, parent eventlog.Store, atSeq int64, count int) (out *v1.ForkResponse, err error) {
	labels, err := encodeMap(req.GetLabels())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "fork labels: %v", err)
	}
	// Keep deletion excluded through publication or durable abort, not just runtime cloning.
	err = s.registry.WithSessionRead(req.GetSession(), func() (err error) {
		info, err := s.store.SessionInfo(req.GetSession())
		if err != nil {
			return err
		}
		if atSeq > info.LastSeq {
			return fmt.Errorf("%w: fork sequence %d exceeds session head %d", placement.ErrUnplaceable, atSeq, info.LastSeq)
		}
		metadata := make([]sqlitelog.SessionMeta, count)
		children := make([]placement.ForkChild, count)
		for i := range metadata {
			uid := newUID()
			var name string
			if len(req.GetChildNames()) != 0 {
				name = req.GetChildNames()[i]
			}
			// Identity is deliberately not inherited; the service refuses per-child principals.
			metadata[i] = sqlitelog.SessionMeta{UID: uid, Project: info.Project, Name: name,
				Harness: info.Harness, Model: info.Model, ParentUID: info.UID, ForkSeq: atSeq,
				Annotations: info.Annotations, Origin: info.Origin, Labels: labels}
			children[i] = placement.ForkChild{UID: uid, Log: s.store.Session(uid)}
		}
		batch, err := s.store.BeginFork(ctx, req.GetSession(), metadata)
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				// Cancellation is a common fork failure. Preserve its cause while durably excluding
				// unfinished children; a failed abort remains PREPARING for startup recovery.
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if abortErr := s.store.AbortFork(cleanupCtx, batch); abortErr != nil {
					err = errors.Join(err, fmt.Errorf("fork: record abort: %w", abortErr))
				}
			}
		}()
		if err := placer.Fork(ctx, parent, req.GetSession(), children, atSeq); err != nil {
			return err
		}
		if err := s.store.CommitFork(ctx, batch); err != nil {
			return err
		}
		committed = true
		out = &v1.ForkResponse{}
		for _, child := range children {
			info, err := s.store.SessionInfo(child.UID)
			if err != nil {
				return err
			}
			childProto, err := sessionProto(info)
			if err != nil {
				return err
			}
			out.Children = append(out.Children, childProto)
		}
		return nil
	})
	if err != nil {
		return nil, forkError(err)
	}
	return out, nil
}
