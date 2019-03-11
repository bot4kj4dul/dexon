// Copyright 2018 The dexon-consensus Authors
// This file is part of the dexon-consensus library.
//
// The dexon-consensus library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus library. If not, see
// <http://www.gnu.org/licenses/>.

package vm

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/dexon-foundation/dexon/accounts/abi"
	"github.com/dexon-foundation/dexon/common"
	"github.com/dexon-foundation/dexon/core/types"
	"github.com/dexon-foundation/dexon/crypto"
	"github.com/dexon-foundation/dexon/params"
	"github.com/dexon-foundation/dexon/rlp"

	coreCommon "github.com/dexon-foundation/dexon-consensus/common"
	dexCore "github.com/dexon-foundation/dexon-consensus/core"
	coreCrypto "github.com/dexon-foundation/dexon-consensus/core/crypto"
	coreUtils "github.com/dexon-foundation/dexon-consensus/core/utils"

	"github.com/dexon-foundation/dexon-consensus/core/crypto/ecdsa"
	coreTypes "github.com/dexon-foundation/dexon-consensus/core/types"
	dkgTypes "github.com/dexon-foundation/dexon-consensus/core/types/dkg"
)

type Bytes32 [32]byte

type ReportType uint64

const (
	ReportTypeInvalidDKG = iota
	ReportTypeForkVote
	ReportTypeForkBlock
)

// Storage position enums.
const (
	roundHeightLoc = iota
	totalSupplyLoc
	totalStakedLoc
	nodesLoc
	nodesOffsetByAddressLoc
	nodesOffsetByNodeKeyAddressLoc
	delegatorsLoc
	delegatorsOffsetLoc
	crsRoundLoc
	crsLoc
	dkgRoundLoc
	dkgResetCountLoc
	dkgMasterPublicKeysLoc
	dkgComplaintsLoc
	dkgReadyLoc
	dkgReadysCountLoc
	dkgFinalizedLoc
	dkgFinalizedsCountLoc
	ownerLoc
	minStakeLoc
	lockupPeriodLoc
	miningVelocityLoc
	nextHalvingSupplyLoc
	lastHalvedAmountLoc
	blockGasLimitLoc
	lambdaBALoc
	lambdaDKGLoc
	notarySetSizeLoc
	dkgSetSizeLoc
	roundLengthLoc
	minBlockIntervalLoc
	fineValuesLoc
	finedRecordsLoc
	minGasPriceLoc
)

func publicKeyToNodeKeyAddress(pkBytes []byte) (common.Address, error) {
	pk, err := crypto.UnmarshalPubkey(pkBytes)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(*pk), nil
}

func idToAddress(id coreTypes.NodeID) common.Address {
	return common.BytesToAddress(id.Hash[12:])
}

// State manipulation helper fro the governance contract.
type GovernanceState struct {
	StateDB StateDB
}

func (s *GovernanceState) getState(loc common.Hash) common.Hash {
	return s.StateDB.GetState(GovernanceContractAddress, loc)
}

func (s *GovernanceState) setState(loc common.Hash, val common.Hash) {
	s.StateDB.SetState(GovernanceContractAddress, loc, val)
}

func (s *GovernanceState) getStateBigInt(loc *big.Int) *big.Int {
	res := s.StateDB.GetState(GovernanceContractAddress, common.BigToHash(loc))
	return new(big.Int).SetBytes(res.Bytes())
}

func (s *GovernanceState) setStateBigInt(loc *big.Int, val *big.Int) {
	s.setState(common.BigToHash(loc), common.BigToHash(val))
}

func (s *GovernanceState) getSlotLoc(loc *big.Int) *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256(common.BigToHash(loc).Bytes()))
}

func (s *GovernanceState) getMapLoc(pos *big.Int, key []byte) *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256(key, common.BigToHash(pos).Bytes()))
}

func (s *GovernanceState) readBytes(loc *big.Int) []byte {
	// Length of the dynamic array (bytes).
	rawLength := s.getStateBigInt(loc)
	lengthByte := new(big.Int).Mod(rawLength, big.NewInt(256))

	// Bytes length <= 31, lengthByte % 2 == 0
	// return the high 31 bytes.
	if new(big.Int).Mod(lengthByte, big.NewInt(2)).Cmp(big.NewInt(0)) == 0 {
		length := new(big.Int).Div(lengthByte, big.NewInt(2)).Uint64()
		return rawLength.Bytes()[:length]
	}

	// Actual length = (rawLength - 1) / 2
	length := new(big.Int).Div(new(big.Int).Sub(rawLength, big.NewInt(1)), big.NewInt(2)).Uint64()

	// Data address.
	dataLoc := s.getSlotLoc(loc)

	// Read continuously for length bytes.
	carry := int64(0)
	if length%32 > 0 {
		carry = 1
	}
	chunks := int64(length/32) + carry
	var data []byte
	for i := int64(0); i < chunks; i++ {
		loc = new(big.Int).Add(dataLoc, big.NewInt(i))
		data = append(data, s.getState(common.BigToHash(loc)).Bytes()...)
	}
	data = data[:length]
	return data
}

func (s *GovernanceState) writeBytes(loc *big.Int, data []byte) {
	length := int64(len(data))

	if length == 0 {
		s.setState(common.BigToHash(loc), common.Hash{})
		return
	}

	// Short bytes (length <= 31).
	if length < 32 {
		data2 := append([]byte(nil), data...)
		// Right pad with zeros
		for len(data2) < 31 {
			data2 = append(data2, byte(0))
		}
		data2 = append(data2, byte(length*2))
		s.setState(common.BigToHash(loc), common.BytesToHash(data2))
		return
	}

	// Write 2 * length + 1.
	storedLength := new(big.Int).Add(new(big.Int).Mul(
		big.NewInt(length), big.NewInt(2)), big.NewInt(1))
	s.setStateBigInt(loc, storedLength)
	// Write data chunck.
	dataLoc := s.getSlotLoc(loc)
	carry := int64(0)
	if length%32 > 0 {
		carry = 1
	}
	chunks := length/32 + carry
	for i := int64(0); i < chunks; i++ {
		loc = new(big.Int).Add(dataLoc, big.NewInt(i))
		maxLoc := (i + 1) * 32
		if maxLoc > length {
			maxLoc = length
		}
		data2 := data[i*32 : maxLoc]
		// Right pad with zeros.
		for len(data2) < 32 {
			data2 = append(data2, byte(0))
		}
		s.setState(common.BigToHash(loc), common.BytesToHash(data2))
	}
}

func (s *GovernanceState) eraseBytes(loc *big.Int) {
	// Length of the dynamic array (bytes).
	rawLength := s.getStateBigInt(loc)
	lengthByte := new(big.Int).Mod(rawLength, big.NewInt(256))

	// Bytes length <= 31, lengthByte % 2 == 0
	// return the high 31 bytes.
	if new(big.Int).Mod(lengthByte, big.NewInt(2)).Cmp(big.NewInt(0)) == 0 {
		s.setStateBigInt(loc, big.NewInt(0))
		return
	}

	// Actual length = (rawLength - 1) / 2
	length := new(big.Int).Div(new(big.Int).Sub(
		rawLength, big.NewInt(1)), big.NewInt(2)).Uint64()

	// Fill 0.
	s.writeBytes(loc, make([]byte, length))

	// Clear slot.
	s.setStateBigInt(loc, big.NewInt(0))
}

func (s *GovernanceState) read1DByteArray(loc *big.Int) [][]byte {
	arrayLength := s.getStateBigInt(loc)
	dataLoc := s.getSlotLoc(loc)

	data := [][]byte{}
	for i := int64(0); i < int64(arrayLength.Uint64()); i++ {
		elementLoc := new(big.Int).Add(dataLoc, big.NewInt(i))
		data = append(data, s.readBytes(elementLoc))
	}

	return data
}

func (s *GovernanceState) appendTo1DByteArray(loc *big.Int, data []byte) {
	// Increase length by 1.
	arrayLength := s.getStateBigInt(loc)
	s.setStateBigInt(loc, new(big.Int).Add(arrayLength, big.NewInt(1)))

	// Write element.
	dataLoc := s.getSlotLoc(loc)
	elementLoc := new(big.Int).Add(dataLoc, arrayLength)
	s.writeBytes(elementLoc, data)
}

func (s *GovernanceState) erase1DByteArray(loc *big.Int) {
	arrayLength := s.getStateBigInt(loc)
	dataLoc := s.getSlotLoc(loc)

	for i := int64(0); i < int64(arrayLength.Uint64()); i++ {
		elementLoc := new(big.Int).Add(dataLoc, big.NewInt(i))
		s.eraseBytes(elementLoc)
	}
	s.setStateBigInt(loc, big.NewInt(0))
}

// uint256[] public roundHeight;
func (s *GovernanceState) RoundHeight(round *big.Int) *big.Int {
	baseLoc := s.getSlotLoc(big.NewInt(roundHeightLoc))
	loc := new(big.Int).Add(baseLoc, round)
	return s.getStateBigInt(loc)
}
func (s *GovernanceState) PushRoundHeight(height *big.Int) {
	// Increase length by 1.
	length := s.getStateBigInt(big.NewInt(roundHeightLoc))
	s.setStateBigInt(big.NewInt(roundHeightLoc), new(big.Int).Add(length, big.NewInt(1)))

	baseLoc := s.getSlotLoc(big.NewInt(roundHeightLoc))
	loc := new(big.Int).Add(baseLoc, length)

	s.setStateBigInt(loc, height)
}

// uint256 public totalSupply;
func (s *GovernanceState) TotalSupply() *big.Int {
	return s.getStateBigInt(big.NewInt(totalSupplyLoc))
}
func (s *GovernanceState) IncTotalSupply(amount *big.Int) {
	s.setStateBigInt(big.NewInt(totalSupplyLoc), new(big.Int).Add(s.TotalSupply(), amount))
}
func (s *GovernanceState) DecTotalSupply(amount *big.Int) {
	s.setStateBigInt(big.NewInt(totalSupplyLoc), new(big.Int).Sub(s.TotalSupply(), amount))
}

// uint256 public totalStaked;
func (s *GovernanceState) TotalStaked() *big.Int {
	return s.getStateBigInt(big.NewInt(totalStakedLoc))
}
func (s *GovernanceState) IncTotalStaked(amount *big.Int) {
	s.setStateBigInt(big.NewInt(totalStakedLoc), new(big.Int).Add(s.TotalStaked(), amount))
}
func (s *GovernanceState) DecTotalStaked(amount *big.Int) {
	s.setStateBigInt(big.NewInt(totalStakedLoc), new(big.Int).Sub(s.TotalStaked(), amount))
}

// struct Node {
//     address owner;
//     bytes publicKey;
//     uint256 staked;
//     uint256 fined;
//     string name;
//     string email;
//     string location;
//     string url;
// }
//
// Node[] nodes;

type nodeInfo struct {
	Owner     common.Address
	PublicKey []byte
	Staked    *big.Int
	Fined     *big.Int
	Name      string
	Email     string
	Location  string
	Url       string
}

const nodeStructSize = 8

func (s *GovernanceState) LenNodes() *big.Int {
	return s.getStateBigInt(big.NewInt(nodesLoc))
}
func (s *GovernanceState) Node(index *big.Int) *nodeInfo {
	node := new(nodeInfo)

	arrayBaseLoc := s.getSlotLoc(big.NewInt(nodesLoc))
	elementBaseLoc := new(big.Int).Add(arrayBaseLoc,
		new(big.Int).Mul(index, big.NewInt(nodeStructSize)))

	// Owner.
	loc := elementBaseLoc
	node.Owner = common.BytesToAddress(s.getState(common.BigToHash(elementBaseLoc)).Bytes())

	// PublicKey.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(1))
	node.PublicKey = s.readBytes(loc)

	// Staked.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(2))
	node.Staked = s.getStateBigInt(loc)

	// Fined.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(3))
	node.Fined = s.getStateBigInt(loc)

	// Name.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(4))
	node.Name = string(s.readBytes(loc))

	// Email.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(5))
	node.Email = string(s.readBytes(loc))

	// Location.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(6))
	node.Location = string(s.readBytes(loc))

	// Url.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(7))
	node.Url = string(s.readBytes(loc))

	return node
}
func (s *GovernanceState) PushNode(n *nodeInfo) {
	// Increase length by 1.
	arrayLength := s.LenNodes()
	s.setStateBigInt(big.NewInt(nodesLoc), new(big.Int).Add(arrayLength, big.NewInt(1)))

	s.UpdateNode(arrayLength, n)
}
func (s *GovernanceState) UpdateNode(index *big.Int, n *nodeInfo) {
	arrayBaseLoc := s.getSlotLoc(big.NewInt(nodesLoc))
	elementBaseLoc := new(big.Int).Add(arrayBaseLoc,
		new(big.Int).Mul(index, big.NewInt(nodeStructSize)))

	// Owner.
	loc := elementBaseLoc
	s.setState(common.BigToHash(loc), n.Owner.Hash())

	// PublicKey.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(1))
	s.writeBytes(loc, n.PublicKey)

	// Staked.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(2))
	s.setStateBigInt(loc, n.Staked)

	// Fined.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(3))
	s.setStateBigInt(loc, n.Fined)

	// Name.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(4))
	s.writeBytes(loc, []byte(n.Name))

	// Email.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(5))
	s.writeBytes(loc, []byte(n.Email))

	// Location.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(6))
	s.writeBytes(loc, []byte(n.Location))

	// Url.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(7))
	s.writeBytes(loc, []byte(n.Url))
}
func (s *GovernanceState) PopLastNode() {
	// Decrease length by 1.
	arrayLength := s.LenNodes()
	newArrayLength := new(big.Int).Sub(arrayLength, big.NewInt(1))
	s.setStateBigInt(big.NewInt(nodesLoc), newArrayLength)

	s.UpdateNode(newArrayLength, &nodeInfo{
		Staked: big.NewInt(0),
		Fined:  big.NewInt(0),
	})
}
func (s *GovernanceState) Nodes() []*nodeInfo {
	var nodes []*nodeInfo
	for i := int64(0); i < int64(s.LenNodes().Uint64()); i++ {
		nodes = append(nodes, s.Node(big.NewInt(i)))
	}
	return nodes
}
func (s *GovernanceState) QualifiedNodes() []*nodeInfo {
	var nodes []*nodeInfo
	for i := int64(0); i < int64(s.LenNodes().Uint64()); i++ {
		node := s.Node(big.NewInt(i))
		if new(big.Int).Sub(node.Staked, node.Fined).Cmp(s.MinStake()) >= 0 {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// mapping(address => uint256) public nodesOffsetByAddress;
func (s *GovernanceState) NodesOffsetByAddress(addr common.Address) *big.Int {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByAddressLoc), addr.Bytes())
	return new(big.Int).Sub(s.getStateBigInt(loc), big.NewInt(1))
}
func (s *GovernanceState) PutNodesOffsetByAddress(addr common.Address, offset *big.Int) {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByAddressLoc), addr.Bytes())
	s.setStateBigInt(loc, new(big.Int).Add(offset, big.NewInt(1)))
}
func (s *GovernanceState) DeleteNodesOffsetByAddress(addr common.Address) {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByAddressLoc), addr.Bytes())
	s.setStateBigInt(loc, big.NewInt(0))
}

// mapping(address => uint256) public nodesOffsetByNodeKeyAddress;
func (s *GovernanceState) NodesOffsetByNodeKeyAddress(addr common.Address) *big.Int {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByNodeKeyAddressLoc), addr.Bytes())
	return new(big.Int).Sub(s.getStateBigInt(loc), big.NewInt(1))
}
func (s *GovernanceState) PutNodesOffsetByNodeKeyAddress(addr common.Address, offset *big.Int) {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByNodeKeyAddressLoc), addr.Bytes())
	s.setStateBigInt(loc, new(big.Int).Add(offset, big.NewInt(1)))
}
func (s *GovernanceState) DeleteNodesOffsetByNodeKeyAddress(addr common.Address) {
	loc := s.getMapLoc(big.NewInt(nodesOffsetByNodeKeyAddressLoc), addr.Bytes())
	s.setStateBigInt(loc, big.NewInt(0))
}

func (s *GovernanceState) PutNodeOffsets(n *nodeInfo, offset *big.Int) error {
	address, err := publicKeyToNodeKeyAddress(n.PublicKey)
	if err != nil {
		return err
	}
	s.PutNodesOffsetByNodeKeyAddress(address, offset)
	s.PutNodesOffsetByAddress(n.Owner, offset)
	return nil
}

func (s *GovernanceState) GetNodeOwnerByID(id coreTypes.NodeID) (common.Address, error) {
	offset := s.NodesOffsetByNodeKeyAddress(idToAddress(id))
	if offset.Cmp(big.NewInt(0)) < 0 {
		return common.Address{}, errors.New("node not found")
	}
	node := s.Node(offset)
	return node.Owner, nil
}

// struct Delegator {
//     address node;
//     address owner;
//     uint256 value;
//     uint256 undelegated_at;
// }

type delegatorInfo struct {
	Owner         common.Address
	Value         *big.Int
	UndelegatedAt *big.Int
}

const delegatorStructSize = 3

// mapping(address => Delegator[]) public delegators;
func (s *GovernanceState) LenDelegators(nodeAddr common.Address) *big.Int {
	loc := s.getMapLoc(big.NewInt(delegatorsLoc), nodeAddr.Bytes())
	return s.getStateBigInt(loc)
}
func (s *GovernanceState) Delegator(nodeAddr common.Address, offset *big.Int) *delegatorInfo {
	delegator := new(delegatorInfo)

	loc := s.getMapLoc(big.NewInt(delegatorsLoc), nodeAddr.Bytes())
	arrayBaseLoc := s.getSlotLoc(loc)
	elementBaseLoc := new(big.Int).Add(arrayBaseLoc, new(big.Int).Mul(big.NewInt(delegatorStructSize), offset))

	// Owner.
	loc = elementBaseLoc
	delegator.Owner = common.BytesToAddress(s.getState(common.BigToHash(elementBaseLoc)).Bytes())

	// Value.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(1))
	delegator.Value = s.getStateBigInt(loc)

	// UndelegatedAt.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(2))
	delegator.UndelegatedAt = s.getStateBigInt(loc)

	return delegator
}
func (s *GovernanceState) PushDelegator(nodeAddr common.Address, delegator *delegatorInfo) {
	// Increase length by 1.
	arrayLength := s.LenDelegators(nodeAddr)
	loc := s.getMapLoc(big.NewInt(delegatorsLoc), nodeAddr.Bytes())
	s.setStateBigInt(loc, new(big.Int).Add(arrayLength, big.NewInt(1)))

	s.UpdateDelegator(nodeAddr, arrayLength, delegator)
}
func (s *GovernanceState) UpdateDelegator(nodeAddr common.Address, offset *big.Int, delegator *delegatorInfo) {
	loc := s.getMapLoc(big.NewInt(delegatorsLoc), nodeAddr.Bytes())
	arrayBaseLoc := s.getSlotLoc(loc)
	elementBaseLoc := new(big.Int).Add(arrayBaseLoc, new(big.Int).Mul(big.NewInt(delegatorStructSize), offset))

	// Owner.
	loc = elementBaseLoc
	s.setState(common.BigToHash(loc), delegator.Owner.Hash())

	// Value.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(1))
	s.setStateBigInt(loc, delegator.Value)

	// UndelegatedAt.
	loc = new(big.Int).Add(elementBaseLoc, big.NewInt(2))
	s.setStateBigInt(loc, delegator.UndelegatedAt)
}
func (s *GovernanceState) PopLastDelegator(nodeAddr common.Address) {
	// Decrease length by 1.
	arrayLength := s.LenDelegators(nodeAddr)
	newArrayLength := new(big.Int).Sub(arrayLength, big.NewInt(1))
	loc := s.getMapLoc(big.NewInt(delegatorsLoc), nodeAddr.Bytes())
	s.setStateBigInt(loc, newArrayLength)

	s.UpdateDelegator(nodeAddr, newArrayLength, &delegatorInfo{
		Value:         big.NewInt(0),
		UndelegatedAt: big.NewInt(0),
	})
}

// mapping(address => mapping(address => uint256)) delegatorsOffset;
func (s *GovernanceState) DelegatorsOffset(nodeAddr, delegatorAddr common.Address) *big.Int {
	loc := s.getMapLoc(s.getMapLoc(big.NewInt(delegatorsOffsetLoc), nodeAddr.Bytes()), delegatorAddr.Bytes())
	return new(big.Int).Sub(s.getStateBigInt(loc), big.NewInt(1))
}
func (s *GovernanceState) PutDelegatorOffset(nodeAddr, delegatorAddr common.Address, offset *big.Int) {
	loc := s.getMapLoc(s.getMapLoc(big.NewInt(delegatorsOffsetLoc), nodeAddr.Bytes()), delegatorAddr.Bytes())
	s.setStateBigInt(loc, new(big.Int).Add(offset, big.NewInt(1)))
}
func (s *GovernanceState) DeleteDelegatorsOffset(nodeAddr, delegatorAddr common.Address) {
	loc := s.getMapLoc(s.getMapLoc(big.NewInt(delegatorsOffsetLoc), nodeAddr.Bytes()), delegatorAddr.Bytes())
	s.setStateBigInt(loc, big.NewInt(0))
}

// uint256 public crsRound;
func (s *GovernanceState) CRSRound() *big.Int {
	return s.getStateBigInt(big.NewInt(crsRoundLoc))
}
func (s *GovernanceState) SetCRSRound(round *big.Int) {
	s.setStateBigInt(big.NewInt(crsRoundLoc), round)
}

// bytes32 public crs;
func (s *GovernanceState) CRS() common.Hash {
	return s.getState(common.BigToHash(big.NewInt(crsLoc)))
}
func (s *GovernanceState) SetCRS(crs common.Hash) {
	s.setState(common.BigToHash(big.NewInt(crsLoc)), crs)
}

// uint256 public dkgRound;
func (s *GovernanceState) DKGRound() *big.Int {
	return s.getStateBigInt(big.NewInt(dkgRoundLoc))
}
func (s *GovernanceState) SetDKGRound(round *big.Int) {
	s.setStateBigInt(big.NewInt(dkgRoundLoc), round)
}

// uint256[] public dkgResetCount;
func (s *GovernanceState) DKGResetCount(round *big.Int) *big.Int {
	arrayBaseLoc := s.getSlotLoc(big.NewInt(dkgResetCountLoc))
	return s.getStateBigInt(new(big.Int).Add(arrayBaseLoc, round))
}
func (s *GovernanceState) IncDKGResetCount(round *big.Int) {
	loc := new(big.Int).Add(s.getSlotLoc(big.NewInt(dkgResetCountLoc)), round)
	count := s.getStateBigInt(loc)
	s.setStateBigInt(loc, new(big.Int).Add(count, big.NewInt(1)))
}

// bytes[] public dkgMasterPublicKeys;
func (s *GovernanceState) DKGMasterPublicKeys() [][]byte {
	return s.read1DByteArray(big.NewInt(dkgMasterPublicKeysLoc))
}
func (s *GovernanceState) PushDKGMasterPublicKey(mpk []byte) {
	s.appendTo1DByteArray(big.NewInt(dkgMasterPublicKeysLoc), mpk)
}
func (s *GovernanceState) UniqueDKGMasterPublicKeys() []*dkgTypes.MasterPublicKey {
	// Prepare DKGMasterPublicKeys.
	var dkgMasterPKs []*dkgTypes.MasterPublicKey
	existence := make(map[coreTypes.NodeID]struct{})
	for _, mpk := range s.DKGMasterPublicKeys() {
		x := new(dkgTypes.MasterPublicKey)
		if err := rlp.DecodeBytes(mpk, x); err != nil {
			panic(err)
		}

		// Only the first DKG MPK submission is valid.
		if _, exists := existence[x.ProposerID]; exists {
			continue
		}
		existence[x.ProposerID] = struct{}{}
		dkgMasterPKs = append(dkgMasterPKs, x)
	}
	return dkgMasterPKs
}
func (s *GovernanceState) GetDKGMasterPublicKeyByProposerID(
	proposerID coreTypes.NodeID) (*dkgTypes.MasterPublicKey, error) {

	for _, mpk := range s.DKGMasterPublicKeys() {
		x := new(dkgTypes.MasterPublicKey)
		if err := rlp.DecodeBytes(mpk, x); err != nil {
			panic(err)
		}
		if x.ProposerID.Equal(proposerID) {
			return x, nil
		}
	}
	return nil, errors.New("not found")
}
func (s *GovernanceState) ClearDKGMasterPublicKeys() {
	s.erase1DByteArray(big.NewInt(dkgMasterPublicKeysLoc))
}

// bytes[] public dkgComplaints;
func (s *GovernanceState) DKGComplaints() [][]byte {
	return s.read1DByteArray(big.NewInt(dkgComplaintsLoc))
}
func (s *GovernanceState) PushDKGComplaint(complaint []byte) {
	s.appendTo1DByteArray(big.NewInt(dkgComplaintsLoc), complaint)
}
func (s *GovernanceState) ClearDKGComplaints() {
	s.erase1DByteArray(big.NewInt(dkgComplaintsLoc))
}

// mapping(address => bool) public dkgReady;
func (s *GovernanceState) DKGMPKReady(addr common.Address) bool {
	mapLoc := s.getMapLoc(big.NewInt(dkgReadyLoc), addr.Bytes())
	return s.getStateBigInt(mapLoc).Cmp(big.NewInt(0)) != 0
}
func (s *GovernanceState) PutDKGMPKReady(addr common.Address, ready bool) {
	mapLoc := s.getMapLoc(big.NewInt(dkgReadyLoc), addr.Bytes())
	res := big.NewInt(0)
	if ready {
		res = big.NewInt(1)
	}
	s.setStateBigInt(mapLoc, res)
}
func (s *GovernanceState) ClearDKGMPKReady(dkgSet map[coreTypes.NodeID]struct{}) {
	for id := range dkgSet {
		s.PutDKGMPKReady(idToAddress(id), false)
	}
}

// uint256 public dkgReadysCount;
func (s *GovernanceState) DKGMPKReadysCount() *big.Int {
	return s.getStateBigInt(big.NewInt(dkgReadysCountLoc))
}
func (s *GovernanceState) IncDKGMPKReadysCount() {
	s.setStateBigInt(big.NewInt(dkgReadysCountLoc),
		new(big.Int).Add(s.getStateBigInt(big.NewInt(dkgReadysCountLoc)), big.NewInt(1)))
}
func (s *GovernanceState) ResetDKGMPKReadysCount() {
	s.setStateBigInt(big.NewInt(dkgReadysCountLoc), big.NewInt(0))
}

// mapping(address => bool) public dkgFinalized;
func (s *GovernanceState) DKGFinalized(addr common.Address) bool {
	mapLoc := s.getMapLoc(big.NewInt(dkgFinalizedLoc), addr.Bytes())
	return s.getStateBigInt(mapLoc).Cmp(big.NewInt(0)) != 0
}
func (s *GovernanceState) PutDKGFinalized(addr common.Address, finalized bool) {
	mapLoc := s.getMapLoc(big.NewInt(dkgFinalizedLoc), addr.Bytes())
	res := big.NewInt(0)
	if finalized {
		res = big.NewInt(1)
	}
	s.setStateBigInt(mapLoc, res)
}
func (s *GovernanceState) ClearDKGFinalized(dkgSet map[coreTypes.NodeID]struct{}) {
	for id := range dkgSet {
		s.PutDKGFinalized(idToAddress(id), false)
	}
}

// uint256 public dkgFinalizedsCount;
func (s *GovernanceState) DKGFinalizedsCount() *big.Int {
	return s.getStateBigInt(big.NewInt(dkgFinalizedsCountLoc))
}
func (s *GovernanceState) IncDKGFinalizedsCount() {
	s.setStateBigInt(big.NewInt(dkgFinalizedsCountLoc),
		new(big.Int).Add(s.getStateBigInt(big.NewInt(dkgFinalizedsCountLoc)), big.NewInt(1)))
}
func (s *GovernanceState) ResetDKGFinalizedsCount() {
	s.setStateBigInt(big.NewInt(dkgFinalizedsCountLoc), big.NewInt(0))
}

// address public owner;
func (s *GovernanceState) Owner() common.Address {
	val := s.getState(common.BigToHash(big.NewInt(ownerLoc)))
	return common.BytesToAddress(val.Bytes())
}
func (s *GovernanceState) SetOwner(newOwner common.Address) {
	s.setState(common.BigToHash(big.NewInt(ownerLoc)), newOwner.Hash())
}

// uint256 public minStake;
func (s *GovernanceState) MinStake() *big.Int {
	return s.getStateBigInt(big.NewInt(minStakeLoc))
}

// uint256 public lockupPeriod;
func (s *GovernanceState) LockupPeriod() *big.Int {
	return s.getStateBigInt(big.NewInt(lockupPeriodLoc))
}

// uint256 public miningVelocity;
func (s *GovernanceState) MiningVelocity() *big.Int {
	return s.getStateBigInt(big.NewInt(miningVelocityLoc))
}
func (s *GovernanceState) HalfMiningVelocity() {
	s.setStateBigInt(big.NewInt(miningVelocityLoc),
		new(big.Int).Div(s.MiningVelocity(), big.NewInt(2)))
}

// uint256 public nextHalvingSupply;
func (s *GovernanceState) NextHalvingSupply() *big.Int {
	return s.getStateBigInt(big.NewInt(nextHalvingSupplyLoc))
}
func (s *GovernanceState) IncNextHalvingSupply(amount *big.Int) {
	s.setStateBigInt(big.NewInt(nextHalvingSupplyLoc),
		new(big.Int).Add(s.NextHalvingSupply(), amount))
}

// uint256 public lastHalvedAmount;
func (s *GovernanceState) LastHalvedAmount() *big.Int {
	return s.getStateBigInt(big.NewInt(lastHalvedAmountLoc))
}
func (s *GovernanceState) HalfLastHalvedAmount() {
	s.setStateBigInt(big.NewInt(lastHalvedAmountLoc),
		new(big.Int).Div(s.LastHalvedAmount(), big.NewInt(2)))
}

func (s *GovernanceState) MiningHalved() {
	s.HalfMiningVelocity()
	s.HalfLastHalvedAmount()
	s.IncNextHalvingSupply(s.LastHalvedAmount())
}

// uint256 public blockGasLimit;
func (s *GovernanceState) BlockGasLimit() *big.Int {
	return s.getStateBigInt(big.NewInt(blockGasLimitLoc))
}
func (s *GovernanceState) SetBlockGasLimit(reward *big.Int) {
	s.setStateBigInt(big.NewInt(blockGasLimitLoc), reward)
}

// uint256 public lambdaBA;
func (s *GovernanceState) LambdaBA() *big.Int {
	return s.getStateBigInt(big.NewInt(lambdaBALoc))
}

// uint256 public lambdaDKG;
func (s *GovernanceState) LambdaDKG() *big.Int {
	return s.getStateBigInt(big.NewInt(lambdaDKGLoc))
}

// uint256 public notarySetSize;
func (s *GovernanceState) NotarySetSize() *big.Int {
	return s.getStateBigInt(big.NewInt(notarySetSizeLoc))
}

// uint256 public dkgSetSize;
func (s *GovernanceState) DKGSetSize() *big.Int {
	return s.getStateBigInt(big.NewInt(dkgSetSizeLoc))
}

// uint256 public roundLength;
func (s *GovernanceState) RoundLength() *big.Int {
	return s.getStateBigInt(big.NewInt(roundLengthLoc))
}

// uint256 public minBlockInterval;
func (s *GovernanceState) MinBlockInterval() *big.Int {
	return s.getStateBigInt(big.NewInt(minBlockIntervalLoc))
}

// uint256[] public fineValues;
func (s *GovernanceState) FineValue(index *big.Int) *big.Int {
	arrayBaseLoc := s.getSlotLoc(big.NewInt(fineValuesLoc))
	return s.getStateBigInt(new(big.Int).Add(arrayBaseLoc, index))
}
func (s *GovernanceState) FineValues() []*big.Int {
	len := s.getStateBigInt(big.NewInt(fineValuesLoc))
	result := make([]*big.Int, len.Uint64())
	for i := 0; i < int(len.Uint64()); i++ {
		result[i] = s.FineValue(big.NewInt(int64(i)))
	}
	return result
}
func (s *GovernanceState) SetFineValues(values []*big.Int) {
	s.setStateBigInt(big.NewInt(fineValuesLoc), big.NewInt(int64(len(values))))

	arrayBaseLoc := s.getSlotLoc(big.NewInt(fineValuesLoc))
	for i, v := range values {
		s.setStateBigInt(new(big.Int).Add(arrayBaseLoc, big.NewInt(int64(i))), v)
	}
}

// mapping(bytes32 => bool) public fineRdecords;
func (s *GovernanceState) FineRecords(recordHash Bytes32) bool {
	loc := s.getMapLoc(big.NewInt(finedRecordsLoc), recordHash[:])
	return s.getStateBigInt(loc).Cmp(big.NewInt(0)) > 0
}
func (s *GovernanceState) SetFineRecords(recordHash Bytes32, status bool) {
	loc := s.getMapLoc(big.NewInt(finedRecordsLoc), recordHash[:])
	value := int64(0)
	if status {
		value = int64(1)
	}
	s.setStateBigInt(loc, big.NewInt(value))
}

// uint256 public minGasPrice;
func (s *GovernanceState) MinGasPrice() *big.Int {
	return s.getStateBigInt(big.NewInt(minGasPriceLoc))
}

// Initialize initializes governance contract state.
func (s *GovernanceState) Initialize(config *params.DexconConfig, totalSupply *big.Int) {
	if config.NextHalvingSupply.Cmp(totalSupply) <= 0 {
		panic(fmt.Sprintf("invalid genesis found, totalSupply: %s, nextHavlingSupply: %s",
			totalSupply, config.NextHalvingSupply))
	}

	// Genesis CRS.
	crs := crypto.Keccak256Hash([]byte(config.GenesisCRSText))
	s.SetCRS(crs)

	// Round 0 height.
	s.PushRoundHeight(big.NewInt(0))

	// Owner.
	s.SetOwner(config.Owner)

	// Governance configuration.
	s.UpdateConfiguration(config)

	// Set totalSupply.
	s.IncTotalSupply(totalSupply)

	// Set DKGRound.
	s.SetDKGRound(big.NewInt(int64(dexCore.DKGDelayRound)))
}

// Stake is a helper function for creating genesis state.
func (s *GovernanceState) Stake(
	addr common.Address, publicKey []byte, staked *big.Int,
	name, email, location, url string) {
	offset := s.LenNodes()
	node := &nodeInfo{
		Owner:     addr,
		PublicKey: publicKey,
		Staked:    staked,
		Fined:     big.NewInt(0),
		Name:      name,
		Email:     email,
		Location:  location,
		Url:       url,
	}
	s.PushNode(node)
	if err := s.PutNodeOffsets(node, offset); err != nil {
		panic(err)
	}

	if staked.Cmp(big.NewInt(0)) == 0 {
		return
	}

	offset = s.LenDelegators(addr)
	s.PushDelegator(addr, &delegatorInfo{
		Owner:         addr,
		Value:         staked,
		UndelegatedAt: big.NewInt(0),
	})
	s.PutDelegatorOffset(addr, addr, offset)

	// Add to network total staked.
	s.IncTotalStaked(staked)
}

const decimalMultiplier = 100000000.0

// Configuration returns the current configuration.
func (s *GovernanceState) Configuration() *params.DexconConfig {
	return &params.DexconConfig{
		MinStake:          s.getStateBigInt(big.NewInt(minStakeLoc)),
		LockupPeriod:      s.getStateBigInt(big.NewInt(lockupPeriodLoc)).Uint64(),
		MiningVelocity:    float32(s.getStateBigInt(big.NewInt(miningVelocityLoc)).Uint64()) / decimalMultiplier,
		NextHalvingSupply: s.getStateBigInt(big.NewInt(nextHalvingSupplyLoc)),
		LastHalvedAmount:  s.getStateBigInt(big.NewInt(lastHalvedAmountLoc)),
		BlockGasLimit:     s.getStateBigInt(big.NewInt(blockGasLimitLoc)).Uint64(),
		LambdaBA:          s.getStateBigInt(big.NewInt(lambdaBALoc)).Uint64(),
		LambdaDKG:         s.getStateBigInt(big.NewInt(lambdaDKGLoc)).Uint64(),
		NotarySetSize:     uint32(s.getStateBigInt(big.NewInt(notarySetSizeLoc)).Uint64()),
		DKGSetSize:        uint32(s.getStateBigInt(big.NewInt(dkgSetSizeLoc)).Uint64()),
		RoundLength:       s.getStateBigInt(big.NewInt(roundLengthLoc)).Uint64(),
		MinBlockInterval:  s.getStateBigInt(big.NewInt(minBlockIntervalLoc)).Uint64(),
		FineValues:        s.FineValues(),
		MinGasPrice:       s.getStateBigInt(big.NewInt(minGasPriceLoc)),
	}
}

// UpdateConfiguration updates system configuration.
func (s *GovernanceState) UpdateConfiguration(cfg *params.DexconConfig) {
	s.setStateBigInt(big.NewInt(minStakeLoc), cfg.MinStake)
	s.setStateBigInt(big.NewInt(lockupPeriodLoc), big.NewInt(int64(cfg.LockupPeriod)))
	s.setStateBigInt(big.NewInt(miningVelocityLoc), big.NewInt(int64(cfg.MiningVelocity*decimalMultiplier)))
	s.setStateBigInt(big.NewInt(nextHalvingSupplyLoc), cfg.NextHalvingSupply)
	s.setStateBigInt(big.NewInt(lastHalvedAmountLoc), cfg.LastHalvedAmount)
	s.setStateBigInt(big.NewInt(blockGasLimitLoc), big.NewInt(int64(cfg.BlockGasLimit)))
	s.setStateBigInt(big.NewInt(lambdaBALoc), big.NewInt(int64(cfg.LambdaBA)))
	s.setStateBigInt(big.NewInt(lambdaDKGLoc), big.NewInt(int64(cfg.LambdaDKG)))
	s.setStateBigInt(big.NewInt(notarySetSizeLoc), big.NewInt(int64(cfg.NotarySetSize)))
	s.setStateBigInt(big.NewInt(dkgSetSizeLoc), big.NewInt(int64(cfg.DKGSetSize)))
	s.setStateBigInt(big.NewInt(roundLengthLoc), big.NewInt(int64(cfg.RoundLength)))
	s.setStateBigInt(big.NewInt(minBlockIntervalLoc), big.NewInt(int64(cfg.MinBlockInterval)))
	s.SetFineValues(cfg.FineValues)
	s.setStateBigInt(big.NewInt(minGasPriceLoc), cfg.MinGasPrice)
}

type rawConfigStruct struct {
	MinStake         *big.Int
	LockupPeriod     *big.Int
	BlockGasLimit    *big.Int
	LambdaBA         *big.Int
	LambdaDKG        *big.Int
	NotarySetSize    *big.Int
	DKGSetSize       *big.Int
	RoundLength      *big.Int
	MinBlockInterval *big.Int
	FineValues       []*big.Int
	MinGasPrice      *big.Int
}

// UpdateConfigurationRaw updates system configuration.
func (s *GovernanceState) UpdateConfigurationRaw(cfg *rawConfigStruct) {
	s.setStateBigInt(big.NewInt(minStakeLoc), cfg.MinStake)
	s.setStateBigInt(big.NewInt(lockupPeriodLoc), cfg.LockupPeriod)
	s.setStateBigInt(big.NewInt(blockGasLimitLoc), cfg.BlockGasLimit)
	s.setStateBigInt(big.NewInt(lambdaBALoc), cfg.LambdaBA)
	s.setStateBigInt(big.NewInt(lambdaDKGLoc), cfg.LambdaDKG)
	s.setStateBigInt(big.NewInt(notarySetSizeLoc), cfg.NotarySetSize)
	s.setStateBigInt(big.NewInt(dkgSetSizeLoc), cfg.DKGSetSize)
	s.setStateBigInt(big.NewInt(roundLengthLoc), cfg.RoundLength)
	s.setStateBigInt(big.NewInt(minBlockIntervalLoc), cfg.MinBlockInterval)
	s.SetFineValues(cfg.FineValues)
	s.setStateBigInt(big.NewInt(minGasPriceLoc), cfg.MinGasPrice)
}

// event ConfigurationChanged();
func (s *GovernanceState) emitConfigurationChangedEvent() {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["ConfigurationChanged"].Id()},
		Data:    []byte{},
	})
}

// event CRSProposed(uint256 round, bytes32 crs);
func (s *GovernanceState) emitCRSProposed(round *big.Int, crs common.Hash) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["CRSProposed"].Id(), common.BigToHash(round)},
		Data:    crs.Bytes(),
	})
}

// event Staked(address indexed NodeAddress, uint256 Amount);
func (s *GovernanceState) emitStaked(nodeAddr common.Address) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Staked"].Id(), nodeAddr.Hash()},
		Data:    []byte{},
	})
}

// event Unstaked(address indexed NodeAddress);
func (s *GovernanceState) emitUnstaked(nodeAddr common.Address) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Unstaked"].Id(), nodeAddr.Hash()},
		Data:    []byte{},
	})
}

// event NodeRemoved(address indexed NodeAddress);
func (s *GovernanceState) emitNodeRemoved(nodeAddr common.Address) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["NodeRemoved"].Id(), nodeAddr.Hash()},
		Data:    []byte{},
	})
}

// event Delegated(address indexed NodeAddress, address indexed DelegatorAddress, uint256 Amount);
func (s *GovernanceState) emitDelegated(nodeAddr, delegatorAddr common.Address, amount *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Delegated"].Id(), nodeAddr.Hash(), delegatorAddr.Hash()},
		Data:    common.BigToHash(amount).Bytes(),
	})
}

// event Undelegated(address indexed NodeAddress, address indexed DelegatorAddress, uint256 Amount);
func (s *GovernanceState) emitUndelegated(nodeAddr, delegatorAddr common.Address, amount *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Undelegated"].Id(), nodeAddr.Hash(), delegatorAddr.Hash()},
		Data:    common.BigToHash(amount).Bytes(),
	})
}

// event Withdrawn(address indexed NodeAddress, address indexed DelegatorAddress, uint256 Amount);
func (s *GovernanceState) emitWithdrawn(nodeAddr common.Address, delegatorAddr common.Address, amount *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Withdrawn"].Id(), nodeAddr.Hash(), delegatorAddr.Hash()},
		Data:    common.BigToHash(amount).Bytes(),
	})
}

// event ForkReported(address indexed NodeAddress, address indexed Type, bytes Arg1, bytes Arg2);
func (s *GovernanceState) emitForkReported(nodeAddr common.Address, reportType *big.Int, arg1, arg2 []byte) {

	t, err := abi.NewType("bytes", nil)
	if err != nil {
		panic(err)
	}

	arg := abi.Arguments{
		abi.Argument{
			Name:    "Arg1",
			Type:    t,
			Indexed: false,
		},
		abi.Argument{
			Name:    "Arg2",
			Type:    t,
			Indexed: false,
		},
	}

	data, err := arg.Pack(arg1, arg2)
	if err != nil {
		panic(err)
	}
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["ForkReported"].Id(), nodeAddr.Hash()},
		Data:    data,
	})
}

// event Fined(address indexed NodeAddress, uint256 Amount);
func (s *GovernanceState) emitFined(nodeAddr common.Address, amount *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["Fined"].Id(), nodeAddr.Hash()},
		Data:    common.BigToHash(amount).Bytes(),
	})
}

// event FinePaid(address indexed NodeAddress, uint256 Amount);
func (s *GovernanceState) emitFinePaid(nodeAddr common.Address, amount *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["FinePaid"].Id(), nodeAddr.Hash()},
		Data:    common.BigToHash(amount).Bytes(),
	})
}

// event DKGReset(uint256 indexed Round, uint256 BlockHeight);
func (s *GovernanceState) emitDKGReset(round *big.Int, blockHeight *big.Int) {
	s.StateDB.AddLog(&types.Log{
		Address: GovernanceContractAddress,
		Topics:  []common.Hash{GovernanceABI.Events["DKGReset"].Id(), common.BigToHash(round)},
		Data:    common.BigToHash(blockHeight).Bytes(),
	})
}

func getRoundState(evm *EVM, round *big.Int) (*GovernanceState, error) {
	gs := &GovernanceState{evm.StateDB}
	height := gs.RoundHeight(round).Uint64()
	if round.Uint64() > dexCore.ConfigRoundShift {
		if height == 0 {
			return nil, errExecutionReverted
		}
	}
	statedb, err := evm.StateAtNumber(height)
	return &GovernanceState{statedb}, err
}

func getConfigState(evm *EVM, round *big.Int) (*GovernanceState, error) {
	configRound := big.NewInt(0)
	if round.Uint64() > dexCore.ConfigRoundShift {
		configRound = new(big.Int).Sub(round, big.NewInt(int64(dexCore.ConfigRoundShift)))
	}
	return getRoundState(evm, configRound)
}

type coreDKGUtils interface {
	SetState(GovernanceState)
	NewGroupPublicKey(*big.Int, int) (tsigVerifierIntf, error)
}
type tsigVerifierIntf interface {
	VerifySignature(coreCommon.Hash, coreCrypto.Signature) bool
}

// GovernanceContract represents the governance contract of DEXCON.
type GovernanceContract struct {
	evm          *EVM
	state        GovernanceState
	contract     *Contract
	coreDKGUtils coreDKGUtils
}

// defaultCoreDKGUtils implements coreDKGUtils.
type defaultCoreDKGUtils struct {
	state GovernanceState
}

func (c *defaultCoreDKGUtils) SetState(state GovernanceState) {
	c.state = state
}

func (c *defaultCoreDKGUtils) NewGroupPublicKey(round *big.Int, threshold int) (tsigVerifierIntf, error) {
	// Prepare DKGMasterPublicKeys.
	mpks := c.state.UniqueDKGMasterPublicKeys()

	// Prepare DKGComplaints.
	var complaints []*dkgTypes.Complaint
	for _, comp := range c.state.DKGComplaints() {
		x := new(dkgTypes.Complaint)
		if err := rlp.DecodeBytes(comp, x); err != nil {
			panic(err)
		}
		complaints = append(complaints, x)
	}

	return dexCore.NewDKGGroupPublicKey(round.Uint64(), mpks, complaints, threshold)
}

func (g *GovernanceContract) Address() common.Address {
	return GovernanceContractAddress
}

func (g *GovernanceContract) transfer(from, to common.Address, amount *big.Int) bool {
	// TODO(w): add this to debug trace so it shows up as internal transaction.
	if g.evm.CanTransfer(g.evm.StateDB, from, amount) {
		g.evm.Transfer(g.evm.StateDB, from, to, amount)
		return true
	}
	return false
}

func (g *GovernanceContract) useGas(gas uint64) ([]byte, error) {
	if !g.contract.UseGas(gas) {
		return nil, ErrOutOfGas
	}
	return nil, nil
}

func (g *GovernanceContract) penalize() ([]byte, error) {
	g.useGas(g.contract.Gas)
	return nil, errExecutionReverted
}

func (g *GovernanceContract) configDKGSetSize(round *big.Int) *big.Int {
	s, err := getConfigState(g.evm, round)
	if err != nil {
		return big.NewInt(0)
	}
	return s.DKGSetSize()
}

func (g *GovernanceContract) getDKGSet(round *big.Int) map[coreTypes.NodeID]struct{} {
	crsRound := g.state.CRSRound()
	var crs common.Hash
	cmp := round.Cmp(crsRound)
	if round.Cmp(big.NewInt(int64(dexCore.DKGDelayRound))) <= 0 {
		state, err := getRoundState(g.evm, big.NewInt(0))
		if err != nil {
			return map[coreTypes.NodeID]struct{}{}
		}
		crs = state.CRS()
		for i := uint64(0); i < round.Uint64(); i++ {
			crs = crypto.Keccak256Hash(crs[:])
		}
	} else if cmp > 0 {
		return map[coreTypes.NodeID]struct{}{}
	} else if cmp == 0 {
		crs = g.state.CRS()
	} else {
		state, err := getRoundState(g.evm, round)
		if err != nil {
			return map[coreTypes.NodeID]struct{}{}
		}
		crs = state.CRS()
	}

	target := coreTypes.NewDKGSetTarget(coreCommon.Hash(crs))
	ns := coreTypes.NewNodeSet()

	state, err := getConfigState(g.evm, round)
	if err != nil {
		return map[coreTypes.NodeID]struct{}{}
	}
	for _, x := range state.QualifiedNodes() {
		mpk, err := ecdsa.NewPublicKeyFromByteSlice(x.PublicKey)
		if err != nil {
			panic(err)
		}
		ns.Add(coreTypes.NewNodeID(mpk))
	}
	return ns.GetSubSet(int(g.configDKGSetSize(round).Uint64()), target)
}

func (g *GovernanceContract) inDKGSet(round *big.Int, nodeID coreTypes.NodeID) bool {
	dkgSet := g.getDKGSet(round)
	_, ok := dkgSet[nodeID]
	return ok
}

func (g *GovernanceContract) clearDKG() {
	dkgSet := g.getDKGSet(g.evm.Round)
	g.state.ClearDKGMasterPublicKeys()
	g.state.ClearDKGComplaints()
	g.state.ClearDKGMPKReady(dkgSet)
	g.state.ResetDKGMPKReadysCount()
	g.state.ClearDKGFinalized(dkgSet)
	g.state.ResetDKGFinalizedsCount()
}

func (g *GovernanceContract) addDKGComplaint(round *big.Int, comp []byte) ([]byte, error) {
	if round.Uint64() != g.evm.Round.Uint64()+1 {
		return nil, errExecutionReverted
	}

	caller := g.contract.Caller()
	offset := g.state.NodesOffsetByNodeKeyAddress(caller)

	// Can not add complaint if caller does not exists.
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	// Finalized caller is not allowed to propose complaint.
	if g.state.DKGFinalized(caller) {
		return g.penalize()
	}

	// Calculate 2f
	threshold := new(big.Int).Mul(
		big.NewInt(2),
		new(big.Int).Div(g.state.DKGSetSize(), big.NewInt(3)))

	// If 2f + 1 of DKG set is finalized, one can not propose complaint anymore.
	if g.state.DKGFinalizedsCount().Cmp(threshold) > 0 {
		return nil, errExecutionReverted
	}

	var dkgComplaint dkgTypes.Complaint
	if err := rlp.DecodeBytes(comp, &dkgComplaint); err != nil {
		return g.penalize()
	}

	// DKGComplaint must belongs to someone in DKG set.
	if !g.inDKGSet(round, dkgComplaint.ProposerID) {
		return g.penalize()
	}

	verified, _ := coreUtils.VerifyDKGComplaintSignature(&dkgComplaint)
	if !verified {
		return g.penalize()
	}

	mpk, err := g.state.GetDKGMasterPublicKeyByProposerID(dkgComplaint.PrivateShare.ProposerID)
	if err != nil {
		return g.penalize()
	}

	// Verify DKG complaint is correct.
	ok, err := coreUtils.VerifyDKGComplaint(&dkgComplaint, mpk)
	if !ok || err != nil {
		return g.penalize()
	}

	// Fine the attacker.
	need, err := coreUtils.NeedPenaltyDKGPrivateShare(&dkgComplaint, mpk)
	if err != nil {
		return g.penalize()
	}
	if need {
		fineValue := g.state.FineValue(big.NewInt(ReportTypeInvalidDKG))
		offset := g.state.NodesOffsetByNodeKeyAddress(idToAddress(dkgComplaint.PrivateShare.ProposerID))
		node := g.state.Node(offset)
		if err := g.fine(node.Owner, fineValue, comp, nil); err != nil {
			return g.penalize()
		}
	}

	g.state.PushDKGComplaint(comp)

	// Set this to relatively high to prevent spamming
	return g.useGas(5000000)
}

func (g *GovernanceContract) addDKGMasterPublicKey(round *big.Int, mpk []byte) ([]byte, error) {
	if round.Uint64() != g.evm.Round.Uint64()+1 {
		return nil, errExecutionReverted
	}

	if g.state.DKGRound().Cmp(g.evm.Round) == 0 {
		// Clear DKG states for next round.
		g.clearDKG()
		g.state.SetDKGRound(round)
	}

	caller := g.contract.Caller()
	offset := g.state.NodesOffsetByNodeKeyAddress(caller)

	// Can not add dkg mpk if not staked.
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	// MPKReady caller is not allowed to propose mpk.
	if g.state.DKGMPKReady(caller) {
		return g.penalize()
	}

	// Calculate 2f
	threshold := new(big.Int).Mul(
		big.NewInt(2),
		new(big.Int).Div(g.state.DKGSetSize(), big.NewInt(3)))

	// If 2f + 1 of DKG set is mpk ready, one can not propose mpk anymore.
	if g.state.DKGMPKReadysCount().Cmp(threshold) > 0 {
		return nil, errExecutionReverted
	}

	var dkgMasterPK dkgTypes.MasterPublicKey
	if err := rlp.DecodeBytes(mpk, &dkgMasterPK); err != nil {
		return g.penalize()
	}

	// DKGMasterPublicKey must belongs to someone in DKG set.
	if !g.inDKGSet(round, dkgMasterPK.ProposerID) {
		return g.penalize()
	}

	verified, _ := coreUtils.VerifyDKGMasterPublicKeySignature(&dkgMasterPK)
	if !verified {
		return g.penalize()
	}

	g.state.PushDKGMasterPublicKey(mpk)
	return g.useGas(100000)
}

func (g *GovernanceContract) addDKGMPKReady(round *big.Int, ready []byte) ([]byte, error) {
	if round.Uint64() != g.evm.Round.Uint64()+1 {
		return nil, errExecutionReverted
	}

	caller := g.contract.Caller()

	var dkgReady dkgTypes.MPKReady
	if err := rlp.DecodeBytes(ready, &dkgReady); err != nil {
		return g.penalize()
	}

	// DKGFInalize must belongs to someone in DKG set.
	if !g.inDKGSet(round, dkgReady.ProposerID) {
		return g.penalize()
	}

	verified, _ := coreUtils.VerifyDKGMPKReadySignature(&dkgReady)
	if !verified {
		return g.penalize()
	}

	if !g.state.DKGMPKReady(caller) {
		g.state.PutDKGMPKReady(caller, true)
		g.state.IncDKGMPKReadysCount()
	}

	return g.useGas(100000)
}

func (g *GovernanceContract) addDKGFinalize(round *big.Int, finalize []byte) ([]byte, error) {
	if round.Uint64() != g.evm.Round.Uint64()+1 {
		return nil, errExecutionReverted
	}

	caller := g.contract.Caller()

	var dkgFinalize dkgTypes.Finalize
	if err := rlp.DecodeBytes(finalize, &dkgFinalize); err != nil {
		return g.penalize()
	}

	// DKGFInalize must belongs to someone in DKG set.
	if !g.inDKGSet(round, dkgFinalize.ProposerID) {
		return g.penalize()
	}

	verified, _ := coreUtils.VerifyDKGFinalizeSignature(&dkgFinalize)
	if !verified {
		return g.penalize()
	}

	if !g.state.DKGFinalized(caller) {
		g.state.PutDKGFinalized(caller, true)
		g.state.IncDKGFinalizedsCount()
	}

	return g.useGas(100000)
}

func (g *GovernanceContract) delegate(nodeAddr common.Address) ([]byte, error) {
	offset := g.state.NodesOffsetByAddress(nodeAddr)
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	caller := g.contract.Caller()
	value := g.contract.Value()

	// Can not delegate if no fund was sent.
	if value.Cmp(big.NewInt(0)) == 0 {
		return nil, errExecutionReverted
	}

	// Can not delegate if already delegated.
	delegatorOffset := g.state.DelegatorsOffset(nodeAddr, caller)
	if delegatorOffset.Cmp(big.NewInt(0)) >= 0 {
		return nil, errExecutionReverted
	}

	// Add to the total staked of node.
	node := g.state.Node(offset)
	node.Staked = new(big.Int).Add(node.Staked, g.contract.Value())
	g.state.UpdateNode(offset, node)

	// Add to network total staked.
	g.state.IncTotalStaked(g.contract.Value())

	// Push delegator record.
	offset = g.state.LenDelegators(nodeAddr)
	g.state.PushDelegator(nodeAddr, &delegatorInfo{
		Owner:         caller,
		Value:         value,
		UndelegatedAt: big.NewInt(0),
	})
	g.state.PutDelegatorOffset(nodeAddr, caller, offset)
	g.state.emitDelegated(nodeAddr, caller, value)

	return g.useGas(200000)
}

func (g *GovernanceContract) updateConfiguration(cfg *rawConfigStruct) ([]byte, error) {
	// Only owner can update configuration.
	if g.contract.Caller() != g.state.Owner() {
		return nil, errExecutionReverted
	}

	g.state.UpdateConfigurationRaw(cfg)
	g.state.emitConfigurationChangedEvent()
	return nil, nil
}

func (g *GovernanceContract) stake(
	publicKey []byte, name, email, location, url string) ([]byte, error) {

	// Reject invalid inputs.
	if len(name) >= 32 || len(email) >= 32 || len(location) >= 32 || len(url) >= 128 {
		return g.penalize()
	}

	caller := g.contract.Caller()
	offset := g.state.NodesOffsetByAddress(caller)

	// Can not stake if already staked.
	if offset.Cmp(big.NewInt(0)) >= 0 {
		return nil, errExecutionReverted
	}

	offset = g.state.LenNodes()
	node := &nodeInfo{
		Owner:     caller,
		PublicKey: publicKey,
		Staked:    big.NewInt(0),
		Fined:     big.NewInt(0),
		Name:      name,
		Email:     email,
		Location:  location,
		Url:       url,
	}
	g.state.PushNode(node)
	if err := g.state.PutNodeOffsets(node, offset); err != nil {
		return g.penalize()
	}

	// Delegate fund to itself.
	if g.contract.Value().Cmp(big.NewInt(0)) > 0 {
		if ret, err := g.delegate(caller); err != nil {
			return ret, err
		}
	}

	g.state.emitStaked(caller)
	return g.useGas(100000)
}

func (g *GovernanceContract) undelegateHelper(nodeAddr, caller common.Address) ([]byte, error) {
	nodeOffset := g.state.NodesOffsetByAddress(nodeAddr)
	if nodeOffset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	offset := g.state.DelegatorsOffset(nodeAddr, caller)
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	node := g.state.Node(nodeOffset)
	if node.Fined.Cmp(big.NewInt(0)) > 0 {
		return nil, errExecutionReverted
	}

	delegator := g.state.Delegator(nodeAddr, offset)

	if delegator.UndelegatedAt.Cmp(big.NewInt(0)) != 0 {
		return nil, errExecutionReverted
	}

	// Set undelegate time.
	delegator.UndelegatedAt = g.evm.Time
	g.state.UpdateDelegator(nodeAddr, offset, delegator)

	// Subtract from the total staked of node.
	node.Staked = new(big.Int).Sub(node.Staked, delegator.Value)
	g.state.UpdateNode(nodeOffset, node)

	// Subtract to network total staked.
	g.state.DecTotalStaked(delegator.Value)

	g.state.emitUndelegated(nodeAddr, caller, delegator.Value)

	return g.useGas(100000)
}

func (g *GovernanceContract) undelegate(nodeAddr common.Address) ([]byte, error) {
	return g.undelegateHelper(nodeAddr, g.contract.Caller())
}

func (g *GovernanceContract) withdraw(nodeAddr common.Address) ([]byte, error) {
	caller := g.contract.Caller()

	nodeOffset := g.state.NodesOffsetByAddress(nodeAddr)
	if nodeOffset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	offset := g.state.DelegatorsOffset(nodeAddr, caller)
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	delegator := g.state.Delegator(nodeAddr, offset)

	// Not yet undelegated.
	if delegator.UndelegatedAt.Cmp(big.NewInt(0)) == 0 {
		return g.penalize()
	}

	unlockTime := new(big.Int).Add(delegator.UndelegatedAt, g.state.LockupPeriod())
	if g.evm.Time.Cmp(unlockTime) <= 0 {
		return g.penalize()
	}

	length := g.state.LenDelegators(nodeAddr)
	lastIndex := new(big.Int).Sub(length, big.NewInt(1))

	// Delete the delegator.
	if offset.Cmp(lastIndex) != 0 {
		lastNode := g.state.Delegator(nodeAddr, lastIndex)
		g.state.UpdateDelegator(nodeAddr, offset, lastNode)
		g.state.PutDelegatorOffset(nodeAddr, lastNode.Owner, offset)
	}
	g.state.DeleteDelegatorsOffset(nodeAddr, caller)
	g.state.PopLastDelegator(nodeAddr)

	// Return the staked fund.
	if !g.transfer(GovernanceContractAddress, delegator.Owner, delegator.Value) {
		return nil, errExecutionReverted
	}

	g.state.emitWithdrawn(nodeAddr, delegator.Owner, delegator.Value)

	// We are the last delegator to withdraw the fund, remove the node info.
	if g.state.LenDelegators(nodeAddr).Cmp(big.NewInt(0)) == 0 {
		length := g.state.LenNodes()
		lastIndex := new(big.Int).Sub(length, big.NewInt(1))

		// Delete the node.
		if offset.Cmp(lastIndex) != 0 {
			lastNode := g.state.Node(lastIndex)
			g.state.UpdateNode(offset, lastNode)
			if err := g.state.PutNodeOffsets(lastNode, offset); err != nil {
				panic(err)
			}
		}
		g.state.DeleteNodesOffsetByAddress(nodeAddr)
		g.state.PopLastNode()
		g.state.emitNodeRemoved(nodeAddr)
	}

	return g.useGas(100000)
}

func (g *GovernanceContract) unstake() ([]byte, error) {
	caller := g.contract.Caller()
	offset := g.state.NodesOffsetByAddress(caller)
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	node := g.state.Node(offset)
	if node.Fined.Cmp(big.NewInt(0)) > 0 {
		return nil, errExecutionReverted
	}

	// Undelegate all delegators.
	lenDelegators := g.state.LenDelegators(caller)
	i := new(big.Int).Sub(lenDelegators, big.NewInt(1))
	for i.Cmp(big.NewInt(0)) >= 0 {
		delegator := g.state.Delegator(caller, i)
		if ret, err := g.undelegateHelper(caller, delegator.Owner); err != nil {
			return ret, err
		}
		i = i.Sub(i, big.NewInt(1))
	}

	g.state.emitUnstaked(caller)

	return g.useGas(100000)
}

func (g *GovernanceContract) payFine(nodeAddr common.Address) ([]byte, error) {
	caller := g.contract.Caller()

	nodeOffset := g.state.NodesOffsetByAddress(nodeAddr)
	if nodeOffset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	offset := g.state.DelegatorsOffset(nodeAddr, caller)
	if offset.Cmp(big.NewInt(0)) < 0 {
		return nil, errExecutionReverted
	}

	node := g.state.Node(nodeOffset)
	if node.Fined.Cmp(big.NewInt(0)) <= 0 || node.Fined.Cmp(g.contract.Value()) < 0 {
		return nil, errExecutionReverted
	}

	node.Fined = new(big.Int).Sub(node.Fined, g.contract.Value())
	g.state.UpdateNode(nodeOffset, node)

	// TODO: paid fine should be added to award pool.

	g.state.emitFinePaid(nodeAddr, g.contract.Value())

	return g.useGas(100000)
}

func (g *GovernanceContract) proposeCRS(nextRound *big.Int, signedCRS []byte) ([]byte, error) {
	if nextRound.Uint64() != g.evm.Round.Uint64()+1 ||
		g.state.CRSRound().Uint64() == nextRound.Uint64() {
		return nil, errExecutionReverted
	}

	prevCRS := g.state.CRS()

	// CRS(n) = hash(CRS(n-1)) if n <= core.DKGRoundDelay
	if g.evm.Round.Uint64() == dexCore.DKGDelayRound {
		for i := uint64(0); i < dexCore.DKGDelayRound; i++ {
			prevCRS = crypto.Keccak256Hash(prevCRS[:])
		}
	}

	threshold := int(g.state.DKGSetSize().Uint64()/3 + 1)
	dkgGPK, err := g.coreDKGUtils.NewGroupPublicKey(nextRound, threshold)
	if err != nil {
		return nil, errExecutionReverted
	}
	signature := coreCrypto.Signature{
		Type:      "bls",
		Signature: signedCRS,
	}
	if !dkgGPK.VerifySignature(coreCommon.Hash(prevCRS), signature) {
		return g.penalize()
	}

	// Save new CRS into state and increase round.
	crs := crypto.Keccak256Hash(signedCRS)

	g.state.SetCRS(crs)
	g.state.SetCRSRound(nextRound)
	g.state.emitCRSProposed(nextRound, crs)

	// To encourage DKG set to propose the correct value, correctly submitting
	// this should cause nothing.
	return g.useGas(0)
}

type sortBytes [][]byte

func (s sortBytes) Less(i, j int) bool {
	return bytes.Compare(s[i], s[j]) < 0
}

func (s sortBytes) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortBytes) Len() int {
	return len(s)
}

func (g *GovernanceContract) fine(nodeAddr common.Address, amount *big.Int, payloads ...[]byte) error {
	sort.Sort(sortBytes(payloads))

	hash := Bytes32(crypto.Keccak256Hash(payloads...))
	if g.state.FineRecords(hash) {
		return errors.New("already fined")
	}
	g.state.SetFineRecords(hash, true)

	nodeOffset := g.state.NodesOffsetByAddress(nodeAddr)
	if nodeOffset.Cmp(big.NewInt(0)) < 0 {
		return errExecutionReverted
	}

	// Set fined value.
	node := g.state.Node(nodeOffset)
	node.Fined = new(big.Int).Add(node.Fined, amount)
	g.state.UpdateNode(nodeOffset, node)

	g.state.emitFined(nodeAddr, amount)

	return nil
}

func (g *GovernanceContract) report(reportType *big.Int, arg1, arg2 []byte) ([]byte, error) {
	typeEnum := ReportType(reportType.Uint64())
	var reportedNodeID coreTypes.NodeID

	switch typeEnum {
	case ReportTypeForkVote:
		vote1 := new(coreTypes.Vote)
		if err := rlp.DecodeBytes(arg1, vote1); err != nil {
			return g.penalize()
		}
		vote2 := new(coreTypes.Vote)
		if err := rlp.DecodeBytes(arg2, vote2); err != nil {
			return g.penalize()
		}
		need, err := coreUtils.NeedPenaltyForkVote(vote1, vote2)
		if !need || err != nil {
			return g.penalize()
		}
		reportedNodeID = vote1.ProposerID
	case ReportTypeForkBlock:
		block1 := new(coreTypes.Block)
		if err := rlp.DecodeBytes(arg1, block1); err != nil {
			return g.penalize()
		}
		block2 := new(coreTypes.Block)
		if err := rlp.DecodeBytes(arg2, block2); err != nil {
			return g.penalize()
		}
		need, err := coreUtils.NeedPenaltyForkBlock(block1, block2)
		if !need || err != nil {
			return g.penalize()
		}
		reportedNodeID = block1.ProposerID
	default:
		return g.penalize()
	}

	offset := g.state.NodesOffsetByNodeKeyAddress(idToAddress(reportedNodeID))
	node := g.state.Node(offset)

	g.state.emitForkReported(node.Owner, reportType, arg1, arg2)

	fineValue := g.state.FineValue(reportType)
	if err := g.fine(node.Owner, fineValue, arg1, arg2); err != nil {
		return nil, errExecutionReverted
	}
	return nil, nil
}

func (g *GovernanceContract) resetDKG(newSignedCRS []byte) ([]byte, error) {
	round := g.evm.Round
	nextRound := new(big.Int).Add(round, big.NewInt(1))

	resetCount := g.state.DKGResetCount(round)

	// Just restart DEXON if failed at round 0.
	if round.Cmp(big.NewInt(0)) == 0 {
		return nil, errExecutionReverted
	}

	// Extend the the current round.
	// target = (80 + 100 * DKGResetCount)%
	target := new(big.Int).Add(
		big.NewInt(80),
		new(big.Int).Mul(big.NewInt(100), resetCount))

	roundHeight := g.state.RoundHeight(round)
	gs, err := getConfigState(g.evm, round)
	if err != nil {
		return nil, err
	}
	config := gs.Configuration()

	targetBlockNum := new(big.Int).SetUint64(config.RoundLength)
	targetBlockNum.Mul(targetBlockNum, target)
	targetBlockNum.Quo(targetBlockNum, big.NewInt(100))
	targetBlockNum.Add(targetBlockNum, roundHeight)

	// Check if current block over 80% of current round.
	blockHeight := g.evm.Context.BlockNumber
	if blockHeight.Cmp(targetBlockNum) < 0 {
		return nil, errExecutionReverted
	}

	// Check if next DKG did not success.
	// Calculate 2f
	threshold := new(big.Int).Mul(
		big.NewInt(2),
		new(big.Int).Div(g.state.DKGSetSize(), big.NewInt(3)))

	// If 2f + 1 of DKG set is finalized, check if DKG succeeded.
	if g.state.DKGFinalizedsCount().Cmp(threshold) > 0 {
		_, err := g.coreDKGUtils.NewGroupPublicKey(nextRound, int(threshold.Int64()))
		// DKG success.
		if err == nil {
			return nil, errExecutionReverted
		}
		switch err {
		case dexCore.ErrNotReachThreshold, dexCore.ErrInvalidThreshold:
		default:
			return nil, errExecutionReverted
		}
	}

	// Update CRS.
	headState, err := getRoundState(g.evm, round)
	if err != nil {
		return nil, errExecutionReverted
	}
	prevCRS := headState.CRS()
	for i := uint64(0); i < resetCount.Uint64()+1; i++ {
		prevCRS = crypto.Keccak256Hash(prevCRS[:])
	}

	dkgGPK, err := g.coreDKGUtils.NewGroupPublicKey(round, int(config.DKGSetSize/3+1))
	if err != nil {
		return nil, errExecutionReverted
	}
	signature := coreCrypto.Signature{
		Type:      "bls",
		Signature: newSignedCRS,
	}
	if !dkgGPK.VerifySignature(coreCommon.Hash(prevCRS), signature) {
		return g.penalize()
	}

	newRound := new(big.Int).Add(g.evm.Round, big.NewInt(1))

	// Clear DKG states for next round.
	g.clearDKG()
	g.state.SetDKGRound(newRound)

	// Save new CRS into state and increase round.
	newCRS := crypto.Keccak256(newSignedCRS)
	crs := common.BytesToHash(newCRS)

	g.state.SetCRS(crs)
	g.state.SetCRSRound(newRound)
	g.state.emitCRSProposed(newRound, crs)

	// Increase reset count.
	g.state.IncDKGResetCount(new(big.Int).Add(round, big.NewInt(1)))

	g.state.emitDKGReset(round, blockHeight)
	return nil, nil
}

// Run executes governance contract.
func (g *GovernanceContract) Run(evm *EVM, input []byte, contract *Contract) (ret []byte, err error) {
	if len(input) < 4 {
		return nil, errExecutionReverted
	}

	// Initialize contract state.
	g.evm = evm
	g.state = GovernanceState{evm.StateDB}
	g.contract = contract
	g.coreDKGUtils.SetState(g.state)

	// Parse input.
	method, exists := GovernanceABI.Sig2Method[string(input[:4])]
	if !exists {
		return nil, errExecutionReverted
	}

	arguments := input[4:]

	// Dispatch method call.
	switch method.Name {
	case "addDKGComplaint":
		args := struct {
			Round     *big.Int
			Complaint []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.addDKGComplaint(args.Round, args.Complaint)
	case "addDKGMasterPublicKey":
		args := struct {
			Round     *big.Int
			PublicKey []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.addDKGMasterPublicKey(args.Round, args.PublicKey)
	case "addDKGMPKReady":
		args := struct {
			Round    *big.Int
			MPKReady []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.addDKGMPKReady(args.Round, args.MPKReady)
	case "addDKGFinalize":
		args := struct {
			Round    *big.Int
			Finalize []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.addDKGFinalize(args.Round, args.Finalize)
	case "delegate":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.delegate(address)
	case "delegatorsLength":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.LenDelegators(address))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "nodesLength":
		res, err := method.Outputs.Pack(g.state.LenNodes())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "payFine":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.payFine(address)
	case "proposeCRS":
		args := struct {
			Round     *big.Int
			SignedCRS []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.proposeCRS(args.Round, args.SignedCRS)
	case "report":
		args := struct {
			Type *big.Int
			Arg1 []byte
			Arg2 []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.report(args.Type, args.Arg1, args.Arg2)
	case "resetDKG":
		args := struct {
			NewSignedCRS []byte
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.resetDKG(args.NewSignedCRS)
	case "stake":
		args := struct {
			PublicKey []byte
			Name      string
			Email     string
			Location  string
			Url       string
		}{}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.stake(args.PublicKey, args.Name, args.Email, args.Location, args.Url)
	case "transferOwnership":
		var newOwner common.Address
		if err := method.Inputs.Unpack(&newOwner, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.transferOwnership(newOwner)
	case "undelegate":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.undelegate(address)
	case "unstake":
		return g.unstake()
	case "updateConfiguration":
		var cfg rawConfigStruct
		if err := method.Inputs.Unpack(&cfg, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.updateConfiguration(&cfg)
	case "withdraw":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		return g.withdraw(address)

	// --------------------------------
	// Solidity auto generated methods.
	// --------------------------------

	case "blockGasLimit":
		res, err := method.Outputs.Pack(g.state.BlockGasLimit())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "crs":
		res, err := method.Outputs.Pack(g.state.CRS())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "crsRound":
		res, err := method.Outputs.Pack(g.state.CRSRound())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "delegators":
		nodeAddr, index := common.Address{}, new(big.Int)
		args := []interface{}{&nodeAddr, &index}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		delegator := g.state.Delegator(nodeAddr, index)
		res, err := method.Outputs.Pack(delegator.Owner, delegator.Value, delegator.UndelegatedAt)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "delegatorsOffset":
		nodeAddr, delegatorAddr := common.Address{}, common.Address{}
		args := []interface{}{&nodeAddr, &delegatorAddr}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.DelegatorsOffset(nodeAddr, delegatorAddr))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgComplaints":
		index := new(big.Int)
		if err := method.Inputs.Unpack(&index, arguments); err != nil {
			return nil, errExecutionReverted
		}
		complaints := g.state.DKGComplaints()
		if int(index.Uint64()) >= len(complaints) {
			return nil, errExecutionReverted
		}
		complaint := complaints[index.Uint64()]
		res, err := method.Outputs.Pack(complaint)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgFinalizeds":
		addr := common.Address{}
		if err := method.Inputs.Unpack(&addr, arguments); err != nil {
			return nil, errExecutionReverted
		}
		finalized := g.state.DKGFinalized(addr)
		res, err := method.Outputs.Pack(finalized)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgFinalizedsCount":
		count := g.state.DKGFinalizedsCount()
		res, err := method.Outputs.Pack(count)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgMasterPublicKeys":
		index := new(big.Int)
		if err := method.Inputs.Unpack(&index, arguments); err != nil {
			return nil, errExecutionReverted
		}
		mpks := g.state.DKGMasterPublicKeys()
		if int(index.Uint64()) >= len(mpks) {
			return nil, errExecutionReverted
		}
		mpk := mpks[index.Uint64()]
		res, err := method.Outputs.Pack(mpk)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgReadys":
		addr := common.Address{}
		if err := method.Inputs.Unpack(&addr, arguments); err != nil {
			return nil, errExecutionReverted
		}
		ready := g.state.DKGMPKReady(addr)
		res, err := method.Outputs.Pack(ready)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgReadysCount":
		count := g.state.DKGMPKReadysCount()
		res, err := method.Outputs.Pack(count)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgResetCount":
		round := new(big.Int)
		if err := method.Inputs.Unpack(&round, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.DKGResetCount(round))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgRound":
		res, err := method.Outputs.Pack(g.state.DKGRound())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "dkgSetSize":
		res, err := method.Outputs.Pack(g.state.DKGSetSize())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "finedRecords":
		record := Bytes32{}
		if err := method.Inputs.Unpack(&record, arguments); err != nil {
			return nil, errExecutionReverted
		}
		value := g.state.FineRecords(record)
		res, err := method.Outputs.Pack(value)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "fineValues":
		index := new(big.Int)
		if err := method.Inputs.Unpack(&index, arguments); err != nil {
			return nil, errExecutionReverted
		}
		value := g.state.FineValue(index)
		res, err := method.Outputs.Pack(value)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "lambdaBA":
		res, err := method.Outputs.Pack(g.state.LambdaBA())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "lambdaDKG":
		res, err := method.Outputs.Pack(g.state.LambdaDKG())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "lastHalvedAmount":
		res, err := method.Outputs.Pack(g.state.LastHalvedAmount())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "lockupPeriod":
		res, err := method.Outputs.Pack(g.state.LockupPeriod())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "minBlockInterval":
		res, err := method.Outputs.Pack(g.state.MinBlockInterval())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "minGasPrice":
		res, err := method.Outputs.Pack(g.state.MinGasPrice())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "miningVelocity":
		res, err := method.Outputs.Pack(g.state.MiningVelocity())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "minStake":
		res, err := method.Outputs.Pack(g.state.MinStake())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "nextHalvingSupply":
		res, err := method.Outputs.Pack(g.state.NextHalvingSupply())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "nodes":
		index := new(big.Int)
		if err := method.Inputs.Unpack(&index, arguments); err != nil {
			return nil, errExecutionReverted
		}
		info := g.state.Node(index)
		res, err := method.Outputs.Pack(
			info.Owner, info.PublicKey, info.Staked, info.Fined,
			info.Name, info.Email, info.Location, info.Url)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "nodesOffsetByAddress":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.NodesOffsetByAddress(address))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "nodesOffsetByNodeKeyAddress":
		address := common.Address{}
		if err := method.Inputs.Unpack(&address, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.NodesOffsetByNodeKeyAddress(address))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "notarySetSize":
		res, err := method.Outputs.Pack(g.state.NotarySetSize())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "owner":
		res, err := method.Outputs.Pack(g.state.Owner())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "roundHeight":
		round := new(big.Int)
		if err := method.Inputs.Unpack(&round, arguments); err != nil {
			return nil, errExecutionReverted
		}
		res, err := method.Outputs.Pack(g.state.RoundHeight(round))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "roundLength":
		res, err := method.Outputs.Pack(g.state.RoundLength())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "totalStaked":
		res, err := method.Outputs.Pack(g.state.TotalStaked())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "totalSupply":
		res, err := method.Outputs.Pack(g.state.TotalSupply())
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	}
	return nil, errExecutionReverted
}

func (g *GovernanceContract) transferOwnership(newOwner common.Address) ([]byte, error) {
	// Only owner can update configuration.
	if g.contract.Caller() != g.state.Owner() {
		return nil, errExecutionReverted
	}
	g.state.SetOwner(newOwner)
	return nil, nil
}

func PackProposeCRS(round uint64, signedCRS []byte) ([]byte, error) {
	method := GovernanceABI.Name2Method["proposeCRS"]
	res, err := method.Inputs.Pack(big.NewInt(int64(round)), signedCRS)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackAddDKGMasterPublicKey(round uint64, mpk *dkgTypes.MasterPublicKey) ([]byte, error) {
	method := GovernanceABI.Name2Method["addDKGMasterPublicKey"]
	encoded, err := rlp.EncodeToBytes(mpk)
	if err != nil {
		return nil, err
	}
	res, err := method.Inputs.Pack(big.NewInt(int64(round)), encoded)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackAddDKGMPKReady(round uint64, ready *dkgTypes.MPKReady) ([]byte, error) {
	method := GovernanceABI.Name2Method["addDKGMPKReady"]
	encoded, err := rlp.EncodeToBytes(ready)
	if err != nil {
		return nil, err
	}
	res, err := method.Inputs.Pack(big.NewInt(int64(round)), encoded)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackAddDKGComplaint(round uint64, complaint *dkgTypes.Complaint) ([]byte, error) {
	method := GovernanceABI.Name2Method["addDKGComplaint"]
	encoded, err := rlp.EncodeToBytes(complaint)
	if err != nil {
		return nil, err
	}

	res, err := method.Inputs.Pack(big.NewInt(int64(round)), encoded)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackAddDKGFinalize(round uint64, final *dkgTypes.Finalize) ([]byte, error) {
	method := GovernanceABI.Name2Method["addDKGFinalize"]
	encoded, err := rlp.EncodeToBytes(final)
	if err != nil {
		return nil, err
	}

	res, err := method.Inputs.Pack(big.NewInt(int64(round)), encoded)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackReportForkVote(vote1, vote2 *coreTypes.Vote) ([]byte, error) {
	method := GovernanceABI.Name2Method["report"]

	vote1Bytes, err := rlp.EncodeToBytes(vote1)
	if err != nil {
		return nil, err
	}

	vote2Bytes, err := rlp.EncodeToBytes(vote2)
	if err != nil {
		return nil, err
	}

	res, err := method.Inputs.Pack(big.NewInt(ReportTypeForkVote), vote1Bytes, vote2Bytes)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackReportForkBlock(block1, block2 *coreTypes.Block) ([]byte, error) {
	method := GovernanceABI.Name2Method["report"]

	block1Bytes, err := rlp.EncodeToBytes(block1)
	if err != nil {
		return nil, err
	}

	block2Bytes, err := rlp.EncodeToBytes(block2)
	if err != nil {
		return nil, err
	}

	res, err := method.Inputs.Pack(big.NewInt(ReportTypeForkBlock), block1Bytes, block2Bytes)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

func PackResetDKG(newSignedCRS []byte) ([]byte, error) {
	method := GovernanceABI.Name2Method["resetDKG"]
	res, err := method.Inputs.Pack(newSignedCRS)
	if err != nil {
		return nil, err
	}
	data := append(method.Id(), res...)
	return data, nil
}

// NodeInfoOracleContract representing a oracle providing the node information.
type NodeInfoOracleContract struct {
}

func (g *NodeInfoOracleContract) Run(evm *EVM, input []byte, contract *Contract) (ret []byte, err error) {
	if len(input) < 4 {
		return nil, errExecutionReverted
	}

	// Parse input.
	method, exists := NodeInfoOracleABI.Sig2Method[string(input[:4])]
	if !exists {
		return nil, errExecutionReverted
	}

	arguments := input[4:]

	// Dispatch method call.
	switch method.Name {
	case "delegators":
		round, nodeAddr, index := new(big.Int), common.Address{}, new(big.Int)
		args := []interface{}{&round, &nodeAddr, &index}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		state, err := getConfigState(evm, round)
		if err != nil {
			return nil, err
		}
		delegator := state.Delegator(nodeAddr, index)
		res, err := method.Outputs.Pack(delegator.Owner, delegator.Value, delegator.UndelegatedAt)
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "delegatorsLength":
		round, address := new(big.Int), common.Address{}
		args := []interface{}{&round, &address}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		state, err := getConfigState(evm, round)
		if err != nil {
			return nil, err
		}
		res, err := method.Outputs.Pack(state.LenDelegators(address))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	case "delegatorsOffset":
		round, nodeAddr, delegatorAddr := new(big.Int), common.Address{}, common.Address{}
		args := []interface{}{&round, &nodeAddr, &delegatorAddr}
		if err := method.Inputs.Unpack(&args, arguments); err != nil {
			return nil, errExecutionReverted
		}
		state, err := getConfigState(evm, round)
		if err != nil {
			return nil, err
		}
		res, err := method.Outputs.Pack(state.DelegatorsOffset(nodeAddr, delegatorAddr))
		if err != nil {
			return nil, errExecutionReverted
		}
		return res, nil
	}
	return nil, errExecutionReverted
}