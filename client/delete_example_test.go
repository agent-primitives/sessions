package client_test

import (
	"context"
	"fmt"

	"github.com/agent-primitives/sessions/client"
)

// Compile the new quickstart snippet in its caller context; it is not run against a server.
func ExampleClient_DeleteSession() {
	_ = func(ctx context.Context, c *client.Client, turn *client.TurnResult) error {
		deleted, err := c.DeleteSession(ctx, turn.Session.GetMetadata().GetUid())
		if err != nil {
			return err
		}
		fmt.Println(deleted.GetDeletionState(), deleted.GetDeletionRequestedTime())
		return nil
	}
}
