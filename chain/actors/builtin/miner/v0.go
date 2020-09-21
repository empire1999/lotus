package miner

import (
	"bytes"
	"errors"

	"github.com/libp2p/go-libp2p-core/peer"
	cbg "github.com/whyrusleeping/cbor-gen"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/go-state-types/dline"
	miner0 "github.com/filecoin-project/specs-actors/actors/builtin/miner"
	adt0 "github.com/filecoin-project/specs-actors/actors/util/adt"

	"github.com/filecoin-project/lotus/chain/actors/adt"
)

var _ State = (*state0)(nil)

type state0 struct {
	miner0.State
	store adt.Store
}

type deadline0 struct {
	miner0.Deadline
	store adt.Store
}

type partition0 struct {
	miner0.Partition
	store adt.Store
}

func (s *state0) AvailableBalance(bal abi.TokenAmount) (abi.TokenAmount, error) {
	return s.GetAvailableBalance(bal), nil
}

func (s *state0) VestedFunds(epoch abi.ChainEpoch) (abi.TokenAmount, error) {
	return s.CheckVestedFunds(s.store, epoch)
}

func (s *state0) LockedFunds() (LockedFunds, error) {
	return LockedFunds{
		VestingFunds:             s.State.LockedFunds,
		InitialPledgeRequirement: s.State.InitialPledgeRequirement,
		PreCommitDeposits:        s.State.PreCommitDeposits,
	}, nil
}

func (s *state0) InitialPledge() (abi.TokenAmount, error) {
	return s.State.InitialPledgeRequirement, nil
}

func (s *state0) PreCommitDeposits() (abi.TokenAmount, error) {
	return s.State.PreCommitDeposits, nil
}

func (s *state0) GetSector(num abi.SectorNumber) (*SectorOnChainInfo, error) {
	info, ok, err := s.State.GetSector(s.store, num)
	if !ok || err != nil {
		return nil, err
	}

	ret := fromV0SectorOnChainInfo(*info)
	return &ret, nil
}

func (s *state0) FindSector(num abi.SectorNumber) (*SectorLocation, error) {
	dlIdx, partIdx, err := s.State.FindSector(s.store, num)
	if err != nil {
		return nil, err
	}
	return &SectorLocation{
		Deadline:  dlIdx,
		Partition: partIdx,
	}, nil
}

// GetSectorExpiration returns the effective expiration of the given sector.
//
// If the sector isn't found or has already been terminated, this method returns
// nil and no error. If the sector does not expire early, the Early expiration
// field is 0.
func (s *state0) GetSectorExpiration(num abi.SectorNumber) (*SectorExpiration, error) {
	dls, err := s.State.LoadDeadlines(s.store)
	if err != nil {
		return nil, err
	}
	// NOTE: this can be optimized significantly.
	// 1. If the sector is non-faulty, it will either expire on-time (can be
	// learned from the sector info), or in the next quantized expiration
	// epoch (i.e., the first element in the partition's expiration queue.
	// 2. If it's faulty, it will expire early within the first 14 entries
	// of the expiration queue.
	stopErr := errors.New("stop")
	out := SectorExpiration{}
	err = dls.ForEach(s.store, func(dlIdx uint64, dl *miner0.Deadline) error {
		partitions, err := dl.PartitionsArray(s.store)
		if err != nil {
			return err
		}
		quant := s.State.QuantSpecForDeadline(dlIdx)
		var part miner0.Partition
		return partitions.ForEach(&part, func(partIdx int64) error {
			if found, err := part.Sectors.IsSet(uint64(num)); err != nil {
				return err
			} else if !found {
				return nil
			}
			if found, err := part.Terminated.IsSet(uint64(num)); err != nil {
				return err
			} else if found {
				// already terminated
				return stopErr
			}

			q, err := miner0.LoadExpirationQueue(s.store, part.ExpirationsEpochs, quant)
			if err != nil {
				return err
			}
			var exp miner0.ExpirationSet
			return q.ForEach(&exp, func(epoch int64) error {
				if early, err := exp.EarlySectors.IsSet(uint64(num)); err != nil {
					return err
				} else if early {
					out.Early = abi.ChainEpoch(epoch)
					return nil
				}
				if onTime, err := exp.OnTimeSectors.IsSet(uint64(num)); err != nil {
					return err
				} else if onTime {
					out.OnTime = abi.ChainEpoch(epoch)
					return stopErr
				}
				return nil
			})
		})
	})
	if err == stopErr {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if out.Early == 0 && out.OnTime == 0 {
		return nil, nil
	}
	return &out, nil
}

func (s *state0) GetPrecommittedSector(num abi.SectorNumber) (*SectorPreCommitOnChainInfo, error) {
	info, ok, err := s.State.GetPrecommittedSector(s.store, num)
	if !ok || err != nil {
		return nil, err
	}

	ret := fromV0SectorPreCommitOnChainInfo(*info)
	return &ret, nil
}

func (s *state0) LoadSectorsFromSet(filter *bitfield.BitField, filterOut bool) (adt.ROnlyArray, error) {
	a, err := adt0.AsArray(s.store, s.State.Sectors)
	if err != nil {
		return nil, err
	}

	incl := func(i uint64) (bool, error) {
		include := true
		if filter != nil {
			set, err := filter.IsSet(i)
			if err != nil {
				return false, xerrors.Errorf("filter check error: %w", err)
			}
			if set == filterOut {
				include = false
			}
		}
		return include, nil
	}

	return &adt.ProxyArray{
		GetFunc: func(idx uint64, v cbor.Unmarshaler) (bool, error) {
			i, err := incl(idx)
			if err != nil {
				return false, err
			}
			if !i {
				return false, nil
			}

			// TODO: ActorUpgrade potentially convert

			return a.Get(idx, v)
		},
		ForEachFunc: func(v cbor.Unmarshaler, fn func(int64) error) error {
			// TODO: ActorUpgrade potentially convert the output
			return a.ForEach(v, func(i int64) error {
				include, err := incl(uint64(i))
				if err != nil {
					return err
				}
				if !include {
					return nil
				}

				return fn(i)
			})
		},
	}, nil
}

func (s *state0) LoadPreCommittedSectors() (adt.Map, error) {
	return adt0.AsMap(s.store, s.State.PreCommittedSectors)
}

func (s *state0) IsAllocated(num abi.SectorNumber) (bool, error) {
	var allocatedSectors bitfield.BitField
	if err := s.store.Get(s.store.Context(), s.State.AllocatedSectors, &allocatedSectors); err != nil {
		return false, err
	}

	return allocatedSectors.IsSet(uint64(num))
}

func (s *state0) LoadDeadline(idx uint64) (Deadline, error) {
	dls, err := s.State.LoadDeadlines(s.store)
	if err != nil {
		return nil, err
	}
	dl, err := dls.LoadDeadline(s.store, idx)
	if err != nil {
		return nil, err
	}
	return &deadline0{*dl, s.store}, nil
}

func (s *state0) ForEachDeadline(cb func(uint64, Deadline) error) error {
	dls, err := s.State.LoadDeadlines(s.store)
	if err != nil {
		return err
	}
	return dls.ForEach(s.store, func(i uint64, dl *miner0.Deadline) error {
		return cb(i, &deadline0{*dl, s.store})
	})
}

func (s *state0) NumDeadlines() (uint64, error) {
	return miner0.WPoStPeriodDeadlines, nil
}

func (s *state0) DeadlinesChanged(other State) bool {
	other0, ok := other.(*state0)
	if !ok {
		// treat an upgrade as a change, always
		return true
	}

	return s.State.Deadlines.Equals(other0.Deadlines)
}

func (s *state0) Info() (MinerInfo, error) {
	info, err := s.State.GetInfo(s.store)
	if err != nil {
		return MinerInfo{}, err
	}

	var pid *peer.ID
	if peerID, err := peer.IDFromBytes(info.PeerId); err == nil {
		pid = &peerID
	}

	mi := MinerInfo{
		Owner:            info.Owner,
		Worker:           info.Worker,
		ControlAddresses: info.ControlAddresses,

		NewWorker:         address.Undef,
		WorkerChangeEpoch: -1,

		PeerId:                     pid,
		Multiaddrs:                 info.Multiaddrs,
		SealProofType:              info.SealProofType,
		SectorSize:                 info.SectorSize,
		WindowPoStPartitionSectors: info.WindowPoStPartitionSectors,
	}

	if info.PendingWorkerKey != nil {
		mi.NewWorker = info.PendingWorkerKey.NewWorker
		mi.WorkerChangeEpoch = info.PendingWorkerKey.EffectiveAt
	}

	return mi, nil
}

func (s *state0) DeadlineInfo(epoch abi.ChainEpoch) *dline.Info {
	return s.State.DeadlineInfo(epoch)
}

func (s *state0) sectors() (adt.Array, error) {
	return adt0.AsArray(s.store, s.Sectors)
}

func (s *state0) decodeSectorOnChainInfo(val *cbg.Deferred) (SectorOnChainInfo, error) {
	var si miner0.SectorOnChainInfo
	err := si.UnmarshalCBOR(bytes.NewReader(val.Raw))
	if err != nil {
		return SectorOnChainInfo{}, err
	}

	return fromV0SectorOnChainInfo(si), nil
}

func (s *state0) precommits() (adt.Map, error) {
	return adt0.AsMap(s.store, s.PreCommittedSectors)
}

func (s *state0) decodeSectorPreCommitOnChainInfo(val *cbg.Deferred) (SectorPreCommitOnChainInfo, error) {
	var sp miner0.SectorPreCommitOnChainInfo
	err := sp.UnmarshalCBOR(bytes.NewReader(val.Raw))
	if err != nil {
		return SectorPreCommitOnChainInfo{}, err
	}

	return fromV0SectorPreCommitOnChainInfo(sp), nil
}

func (d *deadline0) LoadPartition(idx uint64) (Partition, error) {
	p, err := d.Deadline.LoadPartition(d.store, idx)
	if err != nil {
		return nil, err
	}
	return &partition0{*p, d.store}, nil
}

func (d *deadline0) ForEachPartition(cb func(uint64, Partition) error) error {
	ps, err := d.Deadline.PartitionsArray(d.store)
	if err != nil {
		return err
	}
	var part miner0.Partition
	return ps.ForEach(&part, func(i int64) error {
		return cb(uint64(i), &partition0{part, d.store})
	})
}

func (d *deadline0) PartitionsChanged(other Deadline) bool {
	other0, ok := other.(*deadline0)
	if !ok {
		// treat an upgrade as a change, always
		return true
	}

	return d.Deadline.Partitions.Equals(other0.Deadline.Partitions)
}

func (d *deadline0) PostSubmissions() (bitfield.BitField, error) {
	return d.Deadline.PostSubmissions, nil
}

func (p *partition0) AllSectors() (bitfield.BitField, error) {
	return p.Partition.Sectors, nil
}

func (p *partition0) FaultySectors() (bitfield.BitField, error) {
	return p.Partition.Faults, nil
}

func (p *partition0) RecoveringSectors() (bitfield.BitField, error) {
	return p.Partition.Recoveries, nil
}

func fromV0SectorOnChainInfo(v0 miner0.SectorOnChainInfo) SectorOnChainInfo {
	return (SectorOnChainInfo)(v0)
}

func fromV0SectorPreCommitOnChainInfo(v0 miner0.SectorPreCommitOnChainInfo) SectorPreCommitOnChainInfo {
	return (SectorPreCommitOnChainInfo)(v0)
}
