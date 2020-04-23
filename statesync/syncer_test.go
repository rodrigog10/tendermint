package statesync

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	p2pmocks "github.com/tendermint/tendermint/p2p/mocks"
	"github.com/tendermint/tendermint/proxy"
	proxymocks "github.com/tendermint/tendermint/proxy/mocks"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/statesync/mocks"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/tendermint/version"
)

// Sets up a basic syncer that can be used to test OfferSnapshot requests
func setupOfferSyncer(t *testing.T) (*syncer, *proxymocks.AppConnSnapshot) {
	connQuery := &proxymocks.AppConnQuery{}
	connSnapshot := &proxymocks.AppConnSnapshot{}
	stateSource := &mocks.StateSource{}
	stateSource.On("AppHash", mock.Anything).Return([]byte("app_hash"), nil)
	syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")
	return syncer, connSnapshot
}

// Sets up a simple peer mock with an ID
func simplePeer(id string) *p2pmocks.Peer {
	peer := &p2pmocks.Peer{}
	peer.On("ID").Return(p2p.ID(id))
	return peer
}

func TestSyncer_SyncAny(t *testing.T) {
	state := sm.State{
		ChainID: "chain",
		Version: sm.Version{
			Consensus: version.Consensus{
				Block: version.BlockProtocol,
				App:   0,
			},

			Software: version.TMCoreSemVer,
		},

		LastBlockHeight: 1,
		LastBlockID:     types.BlockID{Hash: []byte("blockhash")},
		LastBlockTime:   time.Now(),
		LastResultsHash: []byte("last_results_hash"),
		AppHash:         []byte("app_hash"),

		LastValidators: &types.ValidatorSet{Proposer: &types.Validator{Address: []byte("val1")}},
		Validators:     &types.ValidatorSet{Proposer: &types.Validator{Address: []byte("val2")}},
		NextValidators: &types.ValidatorSet{Proposer: &types.Validator{Address: []byte("val3")}},

		ConsensusParams:                  *types.DefaultConsensusParams(),
		LastHeightConsensusParamsChanged: 1,
	}
	commit := &types.Commit{BlockID: types.BlockID{Hash: []byte("blockhash")}}

	chunks := []*chunk{
		{Height: 1, Format: 1, Index: 0, Chunk: []byte{1, 1, 0}},
		{Height: 1, Format: 1, Index: 1, Chunk: []byte{1, 1, 1}},
		{Height: 1, Format: 1, Index: 2, Chunk: []byte{1, 1, 2}},
	}
	s := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}

	stateSource := &mocks.StateSource{}
	stateSource.On("AppHash", uint64(1)).Return(state.AppHash, nil)
	stateSource.On("AppHash", uint64(2)).Return([]byte("app_hash_2"), nil)
	stateSource.On("Commit", uint64(1)).Return(commit, nil)
	stateSource.On("State", uint64(1)).Return(state, nil)
	connSnapshot := &proxymocks.AppConnSnapshot{}
	connQuery := &proxymocks.AppConnQuery{}

	syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")

	// Adding a chunk should error when no sync is in progress
	_, err := syncer.AddChunk(&chunk{Height: 1, Format: 1, Index: 0, Chunk: []byte{1}})
	require.Error(t, err)

	// Adding a couple of peers should trigger snapshot discovery messages
	peerA := &p2pmocks.Peer{}
	peerA.On("ID").Return(p2p.ID("a"))
	peerA.On("Send", SnapshotChannel, cdc.MustMarshalBinaryBare(&snapshotsRequestMessage{})).Return(true)
	syncer.AddPeer(peerA)
	peerA.AssertExpectations(t)

	peerB := &p2pmocks.Peer{}
	peerB.On("ID").Return(p2p.ID("b"))
	peerB.On("Send", SnapshotChannel, cdc.MustMarshalBinaryBare(&snapshotsRequestMessage{})).Return(true)
	syncer.AddPeer(peerB)
	peerB.AssertExpectations(t)

	// Both peers report back with snapshots. One of them also returns a snapshot we don't want, in
	// format 2, which will be rejected by the ABCI application.
	new, err := syncer.AddSnapshot(peerA, s)
	require.NoError(t, err)
	assert.True(t, new)

	new, err = syncer.AddSnapshot(peerB, s)
	require.NoError(t, err)
	assert.False(t, new)

	new, err = syncer.AddSnapshot(peerB, &snapshot{Height: 2, Format: 2, Chunks: 3, Hash: []byte{1}})
	require.NoError(t, err)
	assert.True(t, new)

	// We start a sync, with peers sending back chunks when requested. We first reject the snapshot
	// with height 2 format 2, and accept the snapshot at height 1.
	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: &abci.Snapshot{
			Height: 2,
			Format: 2,
			Chunks: 3,
			Hash:   []byte{1},
		},
		AppHash: []byte("app_hash_2"),
	}).Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject_format}, nil)
	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: &abci.Snapshot{
			Height:   s.Height,
			Format:   s.Format,
			Chunks:   s.Chunks,
			Hash:     s.Hash,
			Metadata: s.Metadata,
		},
		AppHash: []byte("app_hash"),
	}).Times(2).Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_accept}, nil)

	onChunkRequest := func(args mock.Arguments) {
		msg := &chunkRequestMessage{}
		err := cdc.UnmarshalBinaryBare(args[1].([]byte), &msg)
		require.NoError(t, err)
		require.EqualValues(t, 1, msg.Height)
		require.EqualValues(t, 1, msg.Format)
		require.LessOrEqual(t, msg.Index, uint32(len(chunks)))

		added, err := syncer.AddChunk(chunks[msg.Index])
		require.NoError(t, err)
		assert.True(t, added)
	}
	peerA.On("Send", ChunkChannel, mock.Anything).Maybe().Run(onChunkRequest).Return(true)
	peerB.On("Send", ChunkChannel, mock.Anything).Maybe().Run(onChunkRequest).Return(true)

	// The first time we're applying chunk 1 we tell it to retry the snapshot, which should
	// cause it to reuse the existing chunks but start restoration from the beginning.
	connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
		Index: 1, Chunk: []byte{1, 1, 1},
	}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_retry_snapshot}, nil)

	connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
		Index: 0, Chunk: []byte{1, 1, 0},
	}).Times(2).Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
	connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
		Index: 1, Chunk: []byte{1, 1, 1},
	}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
	connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
		Index: 2, Chunk: []byte{1, 1, 2},
	}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
	connQuery.On("InfoSync", proxy.RequestInfo).Return(&abci.ResponseInfo{
		AppVersion:       9,
		LastBlockHeight:  1,
		LastBlockAppHash: []byte("app_hash"),
	}, nil)

	newState, lastCommit, err := syncer.SyncAny(0)
	require.NoError(t, err)

	// The syncer should have updated the state app version from the ABCI info response.
	expectState := state
	expectState.Version.Consensus.App = 9

	assert.Equal(t, expectState, newState)
	assert.Equal(t, commit, lastCommit)

	connSnapshot.AssertExpectations(t)
	connQuery.AssertExpectations(t)
	peerA.AssertExpectations(t)
	peerB.AssertExpectations(t)
}

func TestSyncer_SyncAny_noSnapshots(t *testing.T) {
	syncer, _ := setupOfferSyncer(t)
	_, _, err := syncer.SyncAny(0)
	assert.Equal(t, errNoSnapshots, err)
}

func TestSyncer_SyncAny_abort(t *testing.T) {
	syncer, connSnapshot := setupOfferSyncer(t)

	s := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	syncer.AddSnapshot(simplePeer("id"), s)
	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_abort}, nil)

	_, _, err := syncer.SyncAny(0)
	assert.Equal(t, errAbort, err)
	connSnapshot.AssertExpectations(t)
}

func TestSyncer_SyncAny_reject(t *testing.T) {
	syncer, connSnapshot := setupOfferSyncer(t)

	// s22 is tried first, then s12, then s11, then errNoSnapshots
	s22 := &snapshot{Height: 2, Format: 2, Chunks: 3, Hash: []byte{1, 2, 3}}
	s12 := &snapshot{Height: 1, Format: 2, Chunks: 3, Hash: []byte{1, 2, 3}}
	s11 := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	syncer.AddSnapshot(simplePeer("id"), s22)
	syncer.AddSnapshot(simplePeer("id"), s12)
	syncer.AddSnapshot(simplePeer("id"), s11)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s22), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject}, nil)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s12), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject}, nil)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s11), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject}, nil)

	_, _, err := syncer.SyncAny(0)
	assert.Equal(t, errNoSnapshots, err)
	connSnapshot.AssertExpectations(t)
}

func TestSyncer_SyncAny_reject_format(t *testing.T) {
	syncer, connSnapshot := setupOfferSyncer(t)

	// s22 is tried first, which reject s22 and s12, then s11 will abort.
	s22 := &snapshot{Height: 2, Format: 2, Chunks: 3, Hash: []byte{1, 2, 3}}
	s12 := &snapshot{Height: 1, Format: 2, Chunks: 3, Hash: []byte{1, 2, 3}}
	s11 := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	syncer.AddSnapshot(simplePeer("id"), s22)
	syncer.AddSnapshot(simplePeer("id"), s12)
	syncer.AddSnapshot(simplePeer("id"), s11)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s22), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject_format}, nil)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s11), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_abort}, nil)

	_, _, err := syncer.SyncAny(0)
	assert.Equal(t, errAbort, err)
	connSnapshot.AssertExpectations(t)
}

func TestSyncer_SyncAny_reject_sender(t *testing.T) {
	syncer, connSnapshot := setupOfferSyncer(t)

	peerA := simplePeer("a")
	peerB := simplePeer("b")
	peerC := simplePeer("c")

	// sbc will be offered first, which will be rejected with reject_sender, causing all snapshots
	// submitted by both b and c (i.e. sb, sc, sbc) to be rejected. Finally, sa will reject and
	// errNoSnapshots is returned.
	sa := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	sb := &snapshot{Height: 2, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	sc := &snapshot{Height: 3, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	sbc := &snapshot{Height: 4, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	_, err := syncer.AddSnapshot(peerA, sa)
	require.NoError(t, err)
	_, err = syncer.AddSnapshot(peerB, sb)
	require.NoError(t, err)
	_, err = syncer.AddSnapshot(peerC, sc)
	require.NoError(t, err)
	_, err = syncer.AddSnapshot(peerB, sbc)
	require.NoError(t, err)
	_, err = syncer.AddSnapshot(peerC, sbc)
	require.NoError(t, err)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(sbc), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject_sender}, nil)

	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(sa), AppHash: []byte("app_hash"),
	}).Once().Return(&abci.ResponseOfferSnapshot{Result: abci.ResponseOfferSnapshot_reject}, nil)

	_, _, err = syncer.SyncAny(0)
	assert.Equal(t, errNoSnapshots, err)
	connSnapshot.AssertExpectations(t)
}

func TestSyncer_SyncAny_abciError(t *testing.T) {
	syncer, connSnapshot := setupOfferSyncer(t)

	errBoom := errors.New("boom")
	s := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}}
	syncer.AddSnapshot(simplePeer("id"), s)
	connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
		Snapshot: toABCI(s), AppHash: []byte("app_hash"),
	}).Once().Return(nil, errBoom)

	_, _, err := syncer.SyncAny(0)
	assert.True(t, errors.Is(err, errBoom))
	connSnapshot.AssertExpectations(t)
}

func TestSyncer_offerSnapshot(t *testing.T) {
	unknownErr := errors.New("unknown error")
	boom := errors.New("boom")

	testcases := map[string]struct {
		result    abci.ResponseOfferSnapshot_Result
		err       error
		expectErr error
	}{
		"accept":         {abci.ResponseOfferSnapshot_accept, nil, nil},
		"abort":          {abci.ResponseOfferSnapshot_abort, nil, errAbort},
		"reject":         {abci.ResponseOfferSnapshot_reject, nil, errRejectSnapshot},
		"reject_format":  {abci.ResponseOfferSnapshot_reject_format, nil, errRejectFormat},
		"reject_sender":  {abci.ResponseOfferSnapshot_reject_sender, nil, errRejectSender},
		"error":          {0, boom, boom},
		"unknown result": {9, nil, unknownErr},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			syncer, connSnapshot := setupOfferSyncer(t)
			s := &snapshot{Height: 1, Format: 1, Chunks: 3, Hash: []byte{1, 2, 3}, trustedAppHash: []byte("app_hash")}
			connSnapshot.On("OfferSnapshotSync", abci.RequestOfferSnapshot{
				Snapshot: toABCI(s),
				AppHash:  []byte("app_hash"),
			}).Return(&abci.ResponseOfferSnapshot{Result: tc.result}, tc.err)
			err := syncer.offerSnapshot(s)
			if tc.expectErr == unknownErr {
				require.Error(t, err)
			} else {
				unwrapped := errors.Unwrap(err)
				if unwrapped != nil {
					err = unwrapped
				}
				assert.Equal(t, tc.expectErr, err)
			}
		})
	}
}

func TestSyncer_applyChunks_Results(t *testing.T) {
	unknownErr := errors.New("unknown error")
	boom := errors.New("boom")

	testcases := map[string]struct {
		result    abci.ResponseApplySnapshotChunk_Result
		err       error
		expectErr error
	}{
		"accept":          {abci.ResponseApplySnapshotChunk_accept, nil, nil},
		"abort":           {abci.ResponseApplySnapshotChunk_abort, nil, errAbort},
		"retry":           {abci.ResponseApplySnapshotChunk_retry, nil, nil},
		"retry_snapshot":  {abci.ResponseApplySnapshotChunk_retry_snapshot, nil, errRetrySnapshot},
		"reject_snapshot": {abci.ResponseApplySnapshotChunk_reject_snapshot, nil, errRejectSnapshot},
		"error":           {0, boom, boom},
		"unknown result":  {9, nil, unknownErr},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			connQuery := &proxymocks.AppConnQuery{}
			connSnapshot := &proxymocks.AppConnSnapshot{}
			stateSource := &mocks.StateSource{}
			stateSource.On("AppHash", mock.Anything).Return([]byte("app_hash"), nil)
			syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")

			body := []byte{1, 2, 3}
			chunks, err := newChunkQueue(&snapshot{Height: 1, Format: 1, Chunks: 1}, "")
			chunks.Add(&chunk{Height: 1, Format: 1, Index: 0, Chunk: body})
			require.NoError(t, err)

			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 0, Chunk: body,
			}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: tc.result}, tc.err)
			if tc.result == abci.ResponseApplySnapshotChunk_retry {
				connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
					Index: 0, Chunk: body,
				}).Once().Return(&abci.ResponseApplySnapshotChunk{
					Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			}

			err = syncer.applyChunks(chunks)
			if tc.expectErr == unknownErr {
				require.Error(t, err)
			} else {
				unwrapped := errors.Unwrap(err)
				if unwrapped != nil {
					err = unwrapped
				}
				assert.Equal(t, tc.expectErr, err)
			}
			connSnapshot.AssertExpectations(t)
		})
	}
}

func TestSyncer_applyChunks_RefetchChunks(t *testing.T) {
	// Discarding chunks via refetch_chunks should work the same for all results
	testcases := map[string]struct {
		result abci.ResponseApplySnapshotChunk_Result
	}{
		"accept":          {abci.ResponseApplySnapshotChunk_accept},
		"abort":           {abci.ResponseApplySnapshotChunk_abort},
		"retry":           {abci.ResponseApplySnapshotChunk_retry},
		"retry_snapshot":  {abci.ResponseApplySnapshotChunk_retry_snapshot},
		"reject_snapshot": {abci.ResponseApplySnapshotChunk_reject_snapshot},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			connQuery := &proxymocks.AppConnQuery{}
			connSnapshot := &proxymocks.AppConnSnapshot{}
			stateSource := &mocks.StateSource{}
			stateSource.On("AppHash", mock.Anything).Return([]byte("app_hash"), nil)
			syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")

			chunks, err := newChunkQueue(&snapshot{Height: 1, Format: 1, Chunks: 3}, "")
			require.NoError(t, err)
			added, err := chunks.Add(&chunk{Height: 1, Format: 1, Index: 0, Chunk: []byte{0}})
			require.True(t, added)
			require.NoError(t, err)
			added, err = chunks.Add(&chunk{Height: 1, Format: 1, Index: 1, Chunk: []byte{1}})
			require.True(t, added)
			require.NoError(t, err)
			added, err = chunks.Add(&chunk{Height: 1, Format: 1, Index: 2, Chunk: []byte{2}})
			require.True(t, added)
			require.NoError(t, err)

			// The first two chunks are accepted, before the last one asks for 1 to be refetched
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 0, Chunk: []byte{0},
			}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 1, Chunk: []byte{1},
			}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 2, Chunk: []byte{2},
			}).Once().Return(&abci.ResponseApplySnapshotChunk{
				Result:        tc.result,
				RefetchChunks: []uint32{1},
			}, nil)

			// Since removing the chunk will cause Next() to block, we spawn a goroutine, then
			// check the queue contents, and finally close the queue to end the goroutine.
			// We don't really care about the result of applyChunks, since it has separate test.
			go func() {
				syncer.applyChunks(chunks)
			}()

			time.Sleep(50 * time.Millisecond)
			assert.True(t, chunks.Has(0))
			assert.False(t, chunks.Has(1))
			assert.True(t, chunks.Has(2))
			err = chunks.Close()
			require.NoError(t, err)
		})
	}
}

func TestSyncer_applyChunks_RejectSenders(t *testing.T) {
	// Banning chunks senders via ban_chunk_senders should work the same for all results
	testcases := map[string]struct {
		result abci.ResponseApplySnapshotChunk_Result
	}{
		"accept":          {abci.ResponseApplySnapshotChunk_accept},
		"abort":           {abci.ResponseApplySnapshotChunk_abort},
		"retry":           {abci.ResponseApplySnapshotChunk_retry},
		"retry_snapshot":  {abci.ResponseApplySnapshotChunk_retry_snapshot},
		"reject_snapshot": {abci.ResponseApplySnapshotChunk_reject_snapshot},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			connQuery := &proxymocks.AppConnQuery{}
			connSnapshot := &proxymocks.AppConnSnapshot{}
			stateSource := &mocks.StateSource{}
			stateSource.On("AppHash", mock.Anything).Return([]byte("app_hash"), nil)
			syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")

			// Set up three peers across two snapshots, and ask for one of them to be banned.
			// It should be banned from all snapshots.
			peerA := simplePeer("a")
			peerB := simplePeer("b")
			peerC := simplePeer("c")

			s1 := &snapshot{Height: 1, Format: 1, Chunks: 3}
			s2 := &snapshot{Height: 2, Format: 1, Chunks: 3}
			syncer.AddSnapshot(peerA, s1)
			syncer.AddSnapshot(peerA, s2)
			syncer.AddSnapshot(peerB, s1)
			syncer.AddSnapshot(peerB, s2)
			syncer.AddSnapshot(peerC, s1)
			syncer.AddSnapshot(peerC, s2)

			chunks, err := newChunkQueue(s1, "")
			require.NoError(t, err)
			added, err := chunks.Add(&chunk{Height: 1, Format: 1, Index: 0, Chunk: []byte{0}, Sender: peerA.ID()})
			require.True(t, added)
			require.NoError(t, err)
			added, err = chunks.Add(&chunk{Height: 1, Format: 1, Index: 1, Chunk: []byte{1}, Sender: peerB.ID()})
			require.True(t, added)
			require.NoError(t, err)
			added, err = chunks.Add(&chunk{Height: 1, Format: 1, Index: 2, Chunk: []byte{2}, Sender: peerC.ID()})
			require.True(t, added)
			require.NoError(t, err)

			// The first two chunks are accepted, before the last one asks for b sender to be rejected
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 0, Chunk: []byte{0}, Sender: "a",
			}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 1, Chunk: []byte{1}, Sender: "b",
			}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
				Index: 2, Chunk: []byte{2}, Sender: "c",
			}).Once().Return(&abci.ResponseApplySnapshotChunk{
				Result:        tc.result,
				RejectSenders: []string{string(peerB.ID())},
			}, nil)

			// On retry, the last chunk will be tried again, so we just accept it then.
			if tc.result == abci.ResponseApplySnapshotChunk_retry {
				connSnapshot.On("ApplySnapshotChunkSync", abci.RequestApplySnapshotChunk{
					Index: 2, Chunk: []byte{2}, Sender: "c",
				}).Once().Return(&abci.ResponseApplySnapshotChunk{Result: abci.ResponseApplySnapshotChunk_accept}, nil)
			}

			// We don't really care about the result of applyChunks, since it has separate test.
			// However, it will block on e.g. retry result, so we spawn a goroutine that will
			// be shut down when the chunk queue closes.
			go func() {
				syncer.applyChunks(chunks)
			}()

			time.Sleep(50 * time.Millisecond)

			s1peers := syncer.snapshots.GetPeers(s1)
			assert.Len(t, s1peers, 2)
			assert.EqualValues(t, "a", s1peers[0].ID())
			assert.EqualValues(t, "c", s1peers[1].ID())

			syncer.snapshots.GetPeers(s1)
			assert.Len(t, s1peers, 2)
			assert.EqualValues(t, "a", s1peers[0].ID())
			assert.EqualValues(t, "c", s1peers[1].ID())

			err = chunks.Close()
			require.NoError(t, err)
		})
	}
}

func TestSyncer_verifyApp(t *testing.T) {
	boom := errors.New("boom")
	s := &snapshot{Height: 3, Format: 1, Chunks: 5, Hash: []byte{1, 2, 3}, trustedAppHash: []byte("app_hash")}

	testcases := map[string]struct {
		response  *abci.ResponseInfo
		err       error
		expectErr error
	}{
		"verified": {&abci.ResponseInfo{
			LastBlockHeight:  3,
			LastBlockAppHash: []byte("app_hash"),
			AppVersion:       9,
		}, nil, nil},
		"invalid height": {&abci.ResponseInfo{
			LastBlockHeight:  5,
			LastBlockAppHash: []byte("app_hash"),
			AppVersion:       9,
		}, nil, errVerifyFailed},
		"invalid hash": {&abci.ResponseInfo{
			LastBlockHeight:  3,
			LastBlockAppHash: []byte("xxx"),
			AppVersion:       9,
		}, nil, errVerifyFailed},
		"error": {nil, boom, boom},
	}
	for name, tc := range testcases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			connQuery := &proxymocks.AppConnQuery{}
			connSnapshot := &proxymocks.AppConnSnapshot{}
			stateSource := &mocks.StateSource{}
			syncer := newSyncer(log.NewNopLogger(), connSnapshot, connQuery, stateSource, "")

			connQuery.On("InfoSync", proxy.RequestInfo).Return(tc.response, tc.err)
			version, err := syncer.verifyApp(s)
			unwrapped := errors.Unwrap(err)
			if unwrapped != nil {
				err = unwrapped
			}
			assert.Equal(t, tc.expectErr, err)
			if err == nil {
				assert.Equal(t, tc.response.AppVersion, version)
			}
		})
	}
}

func toABCI(s *snapshot) *abci.Snapshot {
	return &abci.Snapshot{
		Height:   s.Height,
		Format:   s.Format,
		Chunks:   s.Chunks,
		Hash:     s.Hash,
		Metadata: s.Metadata,
	}
}
