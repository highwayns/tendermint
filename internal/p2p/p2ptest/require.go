package p2ptest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/internal/p2p"
	"github.com/tendermint/tendermint/types"
)

// RequireEmpty requires that the given channel is empty.
func RequireEmpty(t *testing.T, channels ...*p2p.Channel) {
	for _, channel := range channels {
		select {
		case e := <-channel.In:
			require.Fail(t, "unexpected message", "channel %v should be empty, got %v", channel.ID, e)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// RequireReceive requires that the given envelope is received on the channel.
func RequireReceive(t *testing.T, channel *p2p.Channel, expect p2p.Envelope) {
	t.Helper()

	timer := time.NewTimer(time.Second) // not time.After due to goroutine leaks
	defer timer.Stop()

	select {
	case e := <-channel.In:
		require.Equal(t, expect, e)
	case <-timer.C:
		require.Fail(t, "timed out waiting for message", "%v on channel %v", expect, channel.ID)
	}
}

// RequireReceiveUnordered requires that the given envelopes are all received on
// the channel, ignoring order.
func RequireReceiveUnordered(t *testing.T, channel *p2p.Channel, expect []p2p.Envelope) {
	timer := time.NewTimer(time.Second) // not time.After due to goroutine leaks
	defer timer.Stop()

	actual := []p2p.Envelope{}
	for {
		select {
		case e := <-channel.In:
			actual = append(actual, e)
			if len(actual) == len(expect) {
				require.ElementsMatch(t, expect, actual)
				return
			}

		case <-timer.C:
			require.ElementsMatch(t, expect, actual)
			return
		}
	}

}

// RequireSend requires that the given envelope is sent on the channel.
func RequireSend(ctx context.Context, t *testing.T, channel *p2p.Channel, envelope p2p.Envelope) {
	tctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	err := channel.Send(tctx, envelope)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		require.Fail(t, "timed out sending message to %q", envelope.To)
	default:
		require.NoError(t, err, "unexpected error")
	}
}

// RequireSendReceive requires that a given Protobuf message is sent to the
// given peer, and then that the given response is received back.
func RequireSendReceive(
	ctx context.Context,
	t *testing.T,
	channel *p2p.Channel,
	peerID types.NodeID,
	send proto.Message,
	receive proto.Message,
) {
	RequireSend(ctx, t, channel, p2p.Envelope{To: peerID, Message: send})
	RequireReceive(t, channel, p2p.Envelope{From: peerID, Message: send})
}

// RequireNoUpdates requires that a PeerUpdates subscription is empty.
func RequireNoUpdates(ctx context.Context, t *testing.T, peerUpdates *p2p.PeerUpdates) {
	t.Helper()
	select {
	case update := <-peerUpdates.Updates():
		if ctx.Err() == nil {
			require.Fail(t, "unexpected peer updates", "got %v", update)
		}
	case <-ctx.Done():
	default:
	}
}

// RequireError requires that the given peer error is submitted for a peer.
func RequireError(ctx context.Context, t *testing.T, channel *p2p.Channel, peerError p2p.PeerError) {
	tctx, tcancel := context.WithTimeout(ctx, time.Second)
	defer tcancel()

	err := channel.SendError(tctx, peerError)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		require.Fail(t, "timed out reporting error", "%v on %v", peerError, channel.ID)
	default:
		require.NoError(t, err, "unexpected error")
	}
}

// RequireUpdate requires that a PeerUpdates subscription yields the given update.
func RequireUpdate(t *testing.T, peerUpdates *p2p.PeerUpdates, expect p2p.PeerUpdate) {
	timer := time.NewTimer(time.Second) // not time.After due to goroutine leaks
	defer timer.Stop()

	select {
	case update := <-peerUpdates.Updates():
		require.Equal(t, expect, update, "peer update did not match")

	case <-timer.C:
		require.Fail(t, "timed out waiting for peer update", "expected %v", expect)
	}
}

// RequireUpdates requires that a PeerUpdates subscription yields the given updates
// in the given order.
func RequireUpdates(t *testing.T, peerUpdates *p2p.PeerUpdates, expect []p2p.PeerUpdate) {
	timer := time.NewTimer(time.Second) // not time.After due to goroutine leaks
	defer timer.Stop()

	actual := []p2p.PeerUpdate{}
	for {
		select {
		case update := <-peerUpdates.Updates():
			actual = append(actual, update)
			if len(actual) == len(expect) {
				require.Equal(t, expect, actual)
				return
			}

		case <-timer.C:
			require.Equal(t, expect, actual, "did not receive expected peer updates")
			return
		}
	}
}
