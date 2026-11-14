package ateadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Removing classification at any allocation/attach/clone call must lose its neutral
// category; copying peer text into the wrapper must fail the privacy assertion.
func TestComputeFailuresOverAdapter(t *testing.T) {
	ctx := context.Background()
	actor := substrate.ActorRef{Atespace: "space", Name: "session"}
	template := substrate.ObjectRef{Atespace: "space", Name: "template"}
	tag := substrate.SnapshotID{Atespace: "space", Name: "fork-child"}
	operations := map[string]func(*Adapter) error{
		"create": func(ad *Adapter) error { return ad.CreateActor(ctx, actor, template) },
		"resume": func(ad *Adapter) error { _, err := ad.ResumeActor(ctx, actor); return err },
		"get":    func(ad *Adapter) error { _, err := ad.GetActor(ctx, actor); return err },
		"tag":    func(ad *Adapter) error { return ad.TagActor(ctx, actor, tag) },
		"clone":  func(ad *Adapter) error { _, err := ad.CreateActorFromTag(ctx, actor, template, tag); return err },
	}
	for name, call := range operations {
		for _, tc := range []struct {
			code codes.Code
			want error
		}{
			{codes.Unavailable, api.ErrComputeUnavailable},
			{codes.ResourceExhausted, api.ErrComputeResourceExhausted},
			{codes.FailedPrecondition, nil},
			{codes.InvalidArgument, nil},
			{codes.Internal, nil},
			{codes.Unknown, nil},
			{codes.Canceled, nil},
			{codes.DeadlineExceeded, nil},
			{codes.PermissionDenied, nil},
			{codes.Unauthenticated, nil},
		} {
			t.Run(name+"/"+tc.code.String(), func(t *testing.T) {
				peerStatus, err := status.New(tc.code, "https://peer.invalid/?token=credential-marker").WithDetails(
					&errdetails.ErrorInfo{Domain: "agentsessions", Reason: "COMPUTE_CONFIG_INVALID", Metadata: map[string]string{"secret": "credential-marker"}},
				)
				if err != nil {
					t.Fatal(err)
				}
				peer := peerStatus.Err()
				cause := fmt.Errorf("control call: %w", peer)
				ctl := &fakeControl{createErr: cause, resumeErr: cause, getErr: cause, tagErr: cause}
				got := call(FromClient(ctl))
				if !errors.Is(got, cause) || !errors.Is(got, peer) {
					t.Fatalf("lost Go cause: %v", got)
				}
				if tc.want == nil {
					if got != cause {
						t.Fatalf("unknown failure changed: %v", got)
					}
					return
				}
				if !errors.Is(got, tc.want) {
					t.Fatalf("error=%v, want category %v", got, tc.want)
				}
				if strings.Contains(got.Error(), "credential-marker") {
					t.Fatalf("peer text escaped: %v", got)
				}
				// The private Go chain still holds the peer status. The Sessions boundary,
				// not this adapter, must mint fresh public status/details.
				if status.Code(got) != tc.code {
					t.Fatalf("lost peer status in private cause: %v", got)
				}
			})
		}
	}
}

func TestComputeClassificationPreservesLifecycleSentinels(t *testing.T) {
	ctx := context.Background()
	actor := substrate.ActorRef{Atespace: "space", Name: "session"}
	ad := FromClient(&fakeControl{
		createErr: status.Error(codes.AlreadyExists, "exists"),
		resumeErr: status.Error(codes.NotFound, "missing volume or actor"),
		tagErr:    status.Error(codes.AlreadyExists, "exists"),
	})
	if err := ad.CreateActor(ctx, actor, substrate.ObjectRef{}); !errors.Is(err, substrate.ErrActorExists) {
		t.Fatal(err)
	}
	if _, err := ad.ResumeActor(ctx, actor); !errors.Is(err, substrate.ErrActorNotFound) {
		t.Fatal(err)
	}
	if err := ad.TagActor(ctx, actor, substrate.SnapshotID{}); !errors.Is(err, substrate.ErrTagExists) {
		t.Fatal(err)
	}
	ad = FromClient(&fakeControl{resumeErr: status.Error(codes.Aborted, "busy"), tagErr: status.Error(codes.Aborted, "busy")})
	if _, err := ad.ResumeActor(ctx, actor); !errors.Is(err, substrate.ErrActorBusy) {
		t.Fatal(err)
	}
	if err := ad.TagActor(ctx, actor, substrate.SnapshotID{}); !errors.Is(err, substrate.ErrActorBusy) {
		t.Fatal(err)
	}
}
