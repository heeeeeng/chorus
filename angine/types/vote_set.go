// Copyright 2017 ZhongAn Information Technology Services Co.,Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	pbtypes "github.com/Baptist-Publication/chorus/angine/protos/types"
	. "github.com/Baptist-Publication/chorus/module/lib/go-common"
	"github.com/Baptist-Publication/chorus/module/xlib/def"
)

/*
	VoteSet helps collect signatures from validators at each height+round for a
	predefined vote type.

	We need VoteSet to be able to keep track of conflicting votes when validators
	double-sign.  Yet, we can't keep track of *all* the votes seen, as that could
	be a DoS attack vector.

	There are two storage areas for votes.
	1. voteSet.votes
	2. voteSet.votesByBlock

	`.votes` is the "canonical" list of votes.  It always has at least one vote,
	if a vote from a validator had been seen at all.  Usually it keeps track of
	the first vote seen, but when a 2/3 majority is found, votes for that get
	priority and are copied over from `.votesByBlock`.

	`.votesByBlock` keeps track of a list of votes for a particular block.  There
	are two ways a &blockVotes{} gets created in `.votesByBlock`.
	1. the first vote seen by a validator was for the particular block.
	2. a peer claims to have seen 2/3 majority for the particular block.

	Since the first vote from a validator will always get added in `.votesByBlock`
	, all votes in `.votes` will have a corresponding entry in `.votesByBlock`.

	When a &blockVotes{} in `.votesByBlock` reaches a 2/3 majority quorum, its
	votes are copied into `.votes`.

	All this is memory bounded because conflicting votes only get added if a peer
	told us to track that block, each peer only gets to tell us 1 such block, and,
	there's only a limited number of peers.

	NOTE: Assumes that the sum total of voting power does not exceed MaxUInt64.
*/

var (
	ErrVoteUnexpectedStep          = errors.New("Unexpected step")
	ErrVoteInvalidValidatorIndex   = errors.New("Invalid round vote validator index")
	ErrVoteInvalidValidatorAddress = errors.New("Invalid round vote validator address")
	ErrVoteInvalidSignature        = errors.New("Invalid round vote signature")
	ErrVoteMultiSignature          = errors.New("Multi round vote signature")
	ErrVoteInvalidBlockHash        = errors.New("Invalid block hash")
	ErrVoteNilVoteSet              = errors.New("VoteSet is nil")
)

type VoteSet struct {
	chainID string
	height  def.INT
	round   def.INT
	type_   pbtypes.VoteType

	mtx           sync.Mutex
	valSet        *ValidatorSet
	votesBitArray *BitArray
	votes         []*pbtypes.Vote            // Primary votes to share
	sum           int64                      // Sum of voting power for seen votes, discounting conflicts
	maj23         *pbtypes.BlockID           // First 2/3 majority seen
	votesByBlock  map[string]*blockVotes     // string(blockHash|blockParts) -> blockVotes
	peerMaj23s    map[string]pbtypes.BlockID // Maj23 for each peer
}

// Constructs a new VoteSet struct used to accumulate votes for given height/round.
func NewVoteSet(chainID string, height def.INT, round def.INT, type_ pbtypes.VoteType, valSet *ValidatorSet) *VoteSet {
	if height == 0 {
		PanicSanity("Cannot make VoteSet for height == 0, doesn't make sense.")
	}
	vset := &VoteSet{
		chainID:       chainID,
		height:        height,
		round:         round,
		type_:         type_,
		valSet:        valSet,
		votesBitArray: NewBitArray(valSet.Size()),
		votes:         make([]*pbtypes.Vote, valSet.Size()),
		sum:           0,
		maj23:         nil,
		votesByBlock:  make(map[string]*blockVotes, valSet.Size()),
		peerMaj23s:    make(map[string]pbtypes.BlockID),
	}
	for i := range vset.votes {
		vset.votes[i] = &pbtypes.Vote{}
	}
	return vset
}

func (voteSet *VoteSet) ChainID() string {
	return voteSet.chainID
}

func (voteSet *VoteSet) Height() def.INT {
	if voteSet == nil {
		return 0
	}
	return voteSet.height
}

func (voteSet *VoteSet) Round() def.INT {
	if voteSet == nil {
		return -1
	}
	return voteSet.round
}

func (voteSet *VoteSet) Type() pbtypes.VoteType {
	if voteSet == nil {
		return 0x00
	}
	return voteSet.type_
}

func (voteSet *VoteSet) Size() int {
	if voteSet == nil {
		return 0
	}
	return voteSet.valSet.Size()
}

// Returns added=true if vote is valid and new.
// Otherwise returns err=ErrVote[
//		UnexpectedStep | InvalidIndex | InvalidAddress |
//		InvalidSignature | InvalidBlockHash | ConflictingVotes ]
// Duplicate votes return added=false, err=nil.
// Conflicting votes return added=*, err=ErrVoteConflictingVotes.
// NOTE: vote should not be mutated after adding.
// NOTE: VoteSet must not be nil
func (voteSet *VoteSet) AddVote(vote *pbtypes.Vote) (added bool, err error) {
	if voteSet == nil {
		// probably this will never happen
		return false, ErrVoteNilVoteSet
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()

	return voteSet.addVote(vote)
}

// NOTE: Validates as much as possible before attempting to verify the signature.
func (voteSet *VoteSet) addVote(vote *pbtypes.Vote) (added bool, err error) {
	if !vote.Exist() {
		return false, nil
	}
	vdata := vote.GetData()
	valIndex := int(vdata.GetValidatorIndex())
	valAddr := vdata.GetValidatorAddress()
	blockKey := vdata.GetBlockID().Key()

	// Ensure that validator index was set
	if valIndex < 0 || len(valAddr) == 0 {
		return false, ErrVoteInvalidValidatorIndex
	}

	// Make sure the step matches.
	if (vdata.Height != voteSet.height) ||
		(vdata.Round != voteSet.round) ||
		(vdata.Type != voteSet.type_) {
		return false, ErrVoteUnexpectedStep
	}

	// Ensure that signer is a validator.
	lookupAddr, val := voteSet.valSet.GetByIndex(valIndex)
	if val == nil {
		return false, ErrVoteInvalidValidatorIndex
	}

	// Ensure that the signer has the right address
	if !bytes.Equal(valAddr, lookupAddr) {
		return false, ErrVoteInvalidValidatorAddress
	}

	// If we already know of this vote, return false.
	voteSig := NewDefaultSignature(vote.GetSignature())
	if existing, ok := voteSet.getVote(valIndex, blockKey); ok {
		exisSig := NewDefaultSignature(existing.GetSignature())
		if exisSig.Equals(voteSig) {
			return false, nil // duplicate
		} else {
			return false, ErrVoteInvalidSignature // NOTE: assumes deterministic signatures
		}
	}

	// Check signature.
	if !val.PubKey.VerifyBytes(SignBytes(voteSet.chainID, vdata), voteSig) {
		// Bad signature.
		return false, ErrVoteInvalidSignature
	}

	// Add vote and get conflicting vote if any
	added, conflicting := voteSet.addVerifiedVote(vote, blockKey, val.VotingPower)
	if conflicting.Exist() {
		return added, &ErrVoteConflictingVotes{
			VoteA: conflicting,
			VoteB: vote,
		}
	} else {
		if !added {
			PanicSanity("Expected to add non-conflicting vote")
		}
		return added, nil
	}

}

// Returns (vote, true) if vote exists for valIndex and blockKey
func (voteSet *VoteSet) getVote(valIndex int, blockKey string) (vote *pbtypes.Vote, ok bool) {
	if existing := voteSet.votes[valIndex]; existing.Exist() && existing.GetData().GetBlockID().Key() == blockKey {
		return existing, true
	}
	if existing := voteSet.votesByBlock[blockKey].getByIndex(valIndex); existing.Exist() {
		return existing, true
	}
	return nil, false
}

// Assumes signature is valid.
// If conflicting vote exists, returns it.
func (voteSet *VoteSet) addVerifiedVote(vote *pbtypes.Vote, blockKey string, votingPower def.INT) (added bool, conflicting *pbtypes.Vote) {
	vdata := vote.GetData()
	valIndex := int(vdata.ValidatorIndex)

	// Already exists in voteSet.votes?
	if existing := voteSet.votes[valIndex]; existing.Exist() {
		if existing.GetData().GetBlockID().Equals(vdata.BlockID) {
			PanicSanity("addVerifiedVote does not expect duplicate votes")
		} else {
			conflicting = existing
		}
		// Replace vote if blockKey matches voteSet.maj23.
		if voteSet.maj23 != nil && voteSet.maj23.Key() == blockKey {
			voteSet.votes[valIndex] = vote
			voteSet.votesBitArray.SetIndex(valIndex, true)
		}
		// Otherwise don't add it to voteSet.votes
	} else {
		// Add to voteSet.votes and incr .sum
		voteSet.votes[valIndex] = vote
		voteSet.votesBitArray.SetIndex(valIndex, true)
		voteSet.sum += votingPower
	}

	votesByBlock, ok := voteSet.votesByBlock[blockKey]
	if ok {
		if conflicting.Exist() && !votesByBlock.peerMaj23 {
			// There's a conflict and no peer claims that this block is special.
			return false, conflicting
		}
		// We'll add the vote in a bit.
	} else {
		// .votesByBlock doesn't exist...
		if conflicting.Exist() {
			// ... and there's a conflicting vote.
			// We're not even tracking this blockKey, so just forget it.
			return false, conflicting
		} else {
			// ... and there's no conflicting vote.
			// Start tracking this blockKey
			votesByBlock = newBlockVotes(false, voteSet.valSet.Size())
			voteSet.votesByBlock[blockKey] = votesByBlock
			// We'll add the vote in a bit.
		}
	}

	// Before adding to votesByBlock, see if we'll exceed quorum
	origSum := votesByBlock.sum
	quorum := voteSet.valSet.TotalVotingPower()*2/3 + 1

	// Add vote to votesByBlock
	votesByBlock.addVerifiedVote(vote, votingPower)

	// If we just crossed the quorum threshold and have 2/3 majority...
	if origSum < quorum && quorum <= votesByBlock.sum {
		// Only consider the first quorum reached
		if voteSet.maj23 == nil {
			maj23BlockID := *vdata.BlockID
			voteSet.maj23 = &maj23BlockID
			// And also copy votes over to voteSet.votes
			for i, vote := range votesByBlock.votes {
				if vote.Exist() {
					voteSet.votes[i] = vote
				}
			}
		}
	}

	return true, conflicting
}

// If a peer claims that it has 2/3 majority for given blockKey, call this.
// NOTE: if there are too many peers, or too much peer churn,
// this can cause memory issues.
// TODO: implement ability to remove peers too
// NOTE: VoteSet must not be nil
func (voteSet *VoteSet) SetPeerMaj23(peerID string, blockID *pbtypes.BlockID) {
	if voteSet == nil {
		PanicSanity("SetPeerMaj23() on nil VoteSet")
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()

	blockKey := blockID.Key()

	// Make sure peer hasn't already told us something.
	if existing, ok := voteSet.peerMaj23s[peerID]; ok {
		if existing.Equals(blockID) {
			return // Nothing to do
		} else {
			return // TODO bad peer!
		}
	}
	voteSet.peerMaj23s[peerID] = *blockID

	// Create .votesByBlock entry if needed.
	votesByBlock, ok := voteSet.votesByBlock[blockKey]
	if ok {
		if votesByBlock.peerMaj23 {
			return // Nothing to do
		} else {
			votesByBlock.peerMaj23 = true
			// No need to copy votes, already there.
		}
	} else {
		votesByBlock = newBlockVotes(true, voteSet.valSet.Size())
		voteSet.votesByBlock[blockKey] = votesByBlock
		// No need to copy votes, no votes to copy over.
	}
}

func (voteSet *VoteSet) BitArray() *BitArray {
	if voteSet == nil {
		return nil
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return voteSet.votesBitArray.Copy()
}

func (voteSet *VoteSet) BitArrayByBlockID(blockID *pbtypes.BlockID) *BitArray {
	if voteSet == nil || blockID == nil {
		return nil
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	votesByBlock, ok := voteSet.votesByBlock[blockID.Key()]
	if ok {
		return votesByBlock.bitArray.Copy()
	}
	return nil
}

// NOTE: if validator has conflicting votes, returns "canonical" vote
func (voteSet *VoteSet) GetByIndex(valIndex int) *pbtypes.Vote {
	if voteSet == nil {
		return nil
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return voteSet.votes[valIndex]
}

func (voteSet *VoteSet) GetByAddress(address []byte) *pbtypes.Vote {
	if voteSet == nil {
		return nil
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	valIndex, val := voteSet.valSet.GetByAddress(address)
	if val == nil {
		PanicSanity("GetByAddress(address) returned nil")
	}
	return voteSet.votes[valIndex]
}

func (voteSet *VoteSet) HasTwoThirdsMajority() bool {
	if voteSet == nil {
		return false
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return voteSet.maj23 != nil
}

func (voteSet *VoteSet) IsCommit() bool {
	if voteSet == nil {
		return false
	}
	if voteSet.type_ != pbtypes.VoteType_Precommit {
		return false
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return voteSet.maj23 != nil
}

func (voteSet *VoteSet) HasTwoThirdsAny() bool {
	if voteSet == nil {
		return false
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return voteSet.sum > voteSet.valSet.TotalVotingPower()*2/3
}

func (voteSet *VoteSet) HasAll() bool {
	return voteSet.sum == voteSet.valSet.TotalVotingPower()
}

// Returns either a blockhash (or nil) that received +2/3 majority.
// If there exists no such majority, returns (nil, PartSetHeader{}, false).
func (voteSet *VoteSet) TwoThirdsMajority() (blockID pbtypes.BlockID, ok bool) {
	if voteSet == nil {
		return pbtypes.BlockID{}, false
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	if voteSet.maj23 != nil {
		return *voteSet.maj23, true
	} else {
		return pbtypes.BlockID{}, false
	}
}

func (voteSet *VoteSet) String() string {
	if voteSet == nil {
		return "nil-VoteSet"
	}
	return voteSet.StringIndented("")
}

func (voteSet *VoteSet) StringIndented(indent string) string {
	voteStrings := make([]string, len(voteSet.votes))
	for i, vote := range voteSet.votes {
		if vote == nil {
			voteStrings[i] = "nil-Vote"
		} else {
			voteStrings[i] = vote.String()
		}
	}
	return fmt.Sprintf(`VoteSet{
%s  H:%v R:%v T:%v
%s  %v
%s  %v
%s  %v
%s}`,
		indent, voteSet.height, voteSet.round, voteSet.type_,
		indent, strings.Join(voteStrings, "\n"+indent+"  "),
		indent, voteSet.votesBitArray,
		indent, voteSet.peerMaj23s,
		indent)
}

func (voteSet *VoteSet) StringShort() string {
	if voteSet == nil {
		return "nil-VoteSet"
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()
	return fmt.Sprintf(`VoteSet{H:%v R:%v T:%v +2/3:%v %v %v}`,
		voteSet.height,
		voteSet.round,
		voteSet.type_,
		voteSet.maj23.CString(),
		voteSet.votesBitArray,
		voteSet.peerMaj23s)
}

//--------------------------------------------------------------------------------
// Commit

func (voteSet *VoteSet) MakeCommit() *CommitCache {
	if voteSet.type_ != pbtypes.VoteType_Precommit {
		PanicSanity("Cannot MakeCommit() unless VoteSet.Type is VoteTypePrecommit")
	}
	voteSet.mtx.Lock()
	defer voteSet.mtx.Unlock()

	// Make sure we have a 2/3 majority
	if voteSet.maj23 == nil {
		PanicSanity("Cannot MakeCommit() unless a blockhash has +2/3")
	}

	// For every validator, get the precommit
	votesCopy := make([]*pbtypes.Vote, len(voteSet.votes))
	copy(votesCopy, voteSet.votes)
	commit := &pbtypes.Commit{
		BlockID:    voteSet.maj23,
		Precommits: votesCopy,
	}
	return NewCommitCache(commit)
}

//--------------------------------------------------------------------------------

/*
	Votes for a particular block
	There are two ways a *blockVotes gets created for a blockKey.
	1. first (non-conflicting) vote of a validator w/ blockKey (peerMaj23=false)
	2. A peer claims to have a 2/3 majority w/ blockKey (peerMaj23=true)
*/
type blockVotes struct {
	peerMaj23 bool            // peer claims to have maj23
	bitArray  *BitArray       // valIndex -> hasVote?
	votes     []*pbtypes.Vote // valIndex -> *Vote
	sum       def.INT         // vote sum
}

func newBlockVotes(peerMaj23 bool, numValidators int) *blockVotes {
	return &blockVotes{
		peerMaj23: peerMaj23,
		bitArray:  NewBitArray(numValidators),
		votes:     make([]*pbtypes.Vote, numValidators),
		sum:       0,
	}
}

func (vs *blockVotes) addVerifiedVote(vote *pbtypes.Vote, votingPower def.INT) {
	valIndex := int(vote.GetData().ValidatorIndex)
	if existing := vs.votes[valIndex]; existing == nil {
		vs.bitArray.SetIndex(valIndex, true)
		vs.votes[valIndex] = vote
		vs.sum += votingPower
	}
}

func (vs *blockVotes) getByIndex(index int) *pbtypes.Vote {
	if vs == nil {
		return nil
	}
	return vs.votes[index]
}

//--------------------------------------------------------------------------------

// Common interface between *consensus.VoteSet and types.Commit
type VoteSetReader interface {
	Height() def.INT
	Round() def.INT
	Type() pbtypes.VoteType
	Size() int
	BitArray() *BitArray
	GetByIndex(int) *pbtypes.Vote
	IsCommit() bool
}
