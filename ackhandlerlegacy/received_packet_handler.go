package ackhandlerlegacy

import (
	"errors"
	"time"

	"github.com/lucas-clemente/quic-go/frames"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/qerr"
)

var (
	// ErrDuplicatePacket occurres when a duplicate packet is received
	ErrDuplicatePacket = errors.New("ReceivedPacketHandler: Duplicate Packet")
	// ErrPacketSmallerThanLastStopWaiting occurs when a packet arrives with a packet number smaller than the largest LeastUnacked of a StopWaitingFrame. If this error occurs, the packet should be ignored
	ErrPacketSmallerThanLastStopWaiting = errors.New("ReceivedPacketHandler: Packet number smaller than highest StopWaiting")
)

var (
	errInvalidPacketNumber               = errors.New("ReceivedPacketHandler: Invalid packet number")
	errTooManyOutstandingReceivedPackets = qerr.Error(qerr.TooManyOutstandingReceivedPackets, "")
)

type packetHistoryEntry struct {
	EntropyBit   bool
	TimeReceived time.Time
}

type receivedPacketHandler struct {
	highestInOrderObserved        protocol.PacketNumber
	highestInOrderObservedEntropy EntropyAccumulator
	largestObserved               protocol.PacketNumber
	ignorePacketsBelow            protocol.PacketNumber
	currentAckFrame               *frames.AckFrame
	stateChanged                  bool // has an ACK for this state already been sent? Will be set to false every time a new packet arrives, and to false every time an ACK is sent

	packetHistory           map[protocol.PacketNumber]packetHistoryEntry
	smallestInPacketHistory protocol.PacketNumber
}

// NewReceivedPacketHandler creates a new receivedPacketHandler
func NewReceivedPacketHandler() ReceivedPacketHandler {
	return &receivedPacketHandler{
		packetHistory: make(map[protocol.PacketNumber]packetHistoryEntry),
	}
}

func (h *receivedPacketHandler) ReceivedPacket(packetNumber protocol.PacketNumber, entropyBit bool) error {
	if packetNumber == 0 {
		return errInvalidPacketNumber
	}

	// if the packet number is smaller than the largest LeastUnacked value of a StopWaiting we received, we cannot detect if this packet has a duplicate number
	// the packet has to be ignored anyway
	if packetNumber <= h.ignorePacketsBelow {
		return ErrPacketSmallerThanLastStopWaiting
	}

	_, ok := h.packetHistory[packetNumber]
	if packetNumber <= h.highestInOrderObserved || ok {
		return ErrDuplicatePacket
	}

	h.stateChanged = true
	h.currentAckFrame = nil

	if packetNumber > h.largestObserved {
		h.largestObserved = packetNumber
	}

	if packetNumber == h.highestInOrderObserved+1 {
		h.highestInOrderObserved = packetNumber
		h.highestInOrderObservedEntropy.Add(packetNumber, entropyBit)
	}

	h.packetHistory[packetNumber] = packetHistoryEntry{
		EntropyBit:   entropyBit,
		TimeReceived: time.Now(),
	}

	h.garbageCollect()

	if uint32(len(h.packetHistory)) > protocol.MaxTrackedReceivedPackets {
		return errTooManyOutstandingReceivedPackets
	}

	return nil
}

func (h *receivedPacketHandler) ReceivedStopWaiting(f *frames.StopWaitingFrame) error {
	// ignore if StopWaiting is unneeded, because we already received a StopWaiting with a higher LeastUnacked
	if h.ignorePacketsBelow >= f.LeastUnacked {
		return nil
	}

	h.ignorePacketsBelow = f.LeastUnacked - 1

	// ignore if StopWaiting is unneeded, since all packets below have already been received
	if h.highestInOrderObserved >= f.LeastUnacked {
		return nil
	}

	// the LeastUnacked is the smallest packet number of any packet for which the sender is still awaiting an ack. So the highestInOrderObserved is one less than that
	h.highestInOrderObserved = f.LeastUnacked - 1
	h.highestInOrderObservedEntropy = EntropyAccumulator(f.Entropy)

	h.garbageCollect()

	return nil
}

// getNackRanges gets all the NACK ranges
func (h *receivedPacketHandler) getNackRanges() ([]frames.NackRange, EntropyAccumulator) {
	// TODO: use a better data structure here
	var ranges []frames.NackRange
	inRange := false
	entropy := h.highestInOrderObservedEntropy
	for i := h.largestObserved; i > h.highestInOrderObserved; i-- {
		p, ok := h.packetHistory[i]
		if !ok {
			if !inRange {
				r := frames.NackRange{
					FirstPacketNumber: i,
					LastPacketNumber:  i,
				}
				ranges = append(ranges, r)
				inRange = true
			} else {
				ranges[len(ranges)-1].FirstPacketNumber--
			}
		} else {
			inRange = false
			entropy.Add(i, p.EntropyBit)
		}
	}
	return ranges, entropy
}

func (h *receivedPacketHandler) GetAckFrame(dequeue bool) (*frames.AckFrame, error) {
	if !h.stateChanged {
		return nil, nil
	}

	if dequeue {
		h.stateChanged = false
	}

	if h.currentAckFrame != nil {
		return h.currentAckFrame, nil
	}

	p, ok := h.packetHistory[h.largestObserved]
	if !ok {
		return nil, ErrMapAccess
	}
	packetReceivedTime := p.TimeReceived

	nackRanges, entropy := h.getNackRanges()
	h.currentAckFrame = &frames.AckFrame{
		AckFrameLegacy: &frames.AckFrameLegacy{
			LargestObserved:    h.largestObserved,
			Entropy:            byte(entropy),
			NackRanges:         nackRanges,
			PacketReceivedTime: packetReceivedTime,
		},
	}

	return h.currentAckFrame, nil
}

func (h *receivedPacketHandler) garbageCollect() {
	for i := h.smallestInPacketHistory; i < h.highestInOrderObserved; i++ {
		delete(h.packetHistory, i)
	}
	h.smallestInPacketHistory = h.highestInOrderObserved
}
