package fmfm

import (
	"fmt"
	"math"
	"time"

	"github.com/but80/fmfm/ymf"
	"github.com/but80/fmfm/ymf/ymfdata"
	"github.com/but80/smaf825/pb/smaf"
)

type flag int

const (
	flagSustain flag = 0x02
	flagVibrato flag = 0x04
	flagFree    flag = 0x80
)

const modThresh = 40

const (
	ccBankMSB      = 0
	ccModulation   = 1
	ccDataEntryHi  = 6
	ccVolume       = 7
	ccPan          = 10
	ccExpression   = 11
	ccBankLSB      = 32
	ccDataEntryLo  = 38
	ccSustainPedal = 64
	ccSoftPedal    = 67
	ccReverb       = 91
	ccChorus       = 93
	ccNRPNLo       = 98
	ccNRPNHi       = 99
	ccRPNLo        = 100
	ccRPNHi        = 101
	ccSoundsOff    = 120
	ccNotesOff     = 123
	ccMono         = 126
	ccPoly         = 127
)

type slot struct {
	midiChannel int
	note        int
	realnote    int
	flags       flag
	finetune    int
	pitch       int
	velocity    int
	instrument  *smaf.VM35VoicePC
	time        time.Time
}

type midiChannelState struct {
	bankLSB    uint8
	bankMSB    uint8
	pc         uint8
	volume     uint8
	expression uint8
	pan        uint8
	pitch      int8
	sustain    uint8
	modulation uint8
	pitchSens  uint16
	rpn        uint16
}

// Controller は、MIDIに類似するインタフェースで Chip のレジスタをコントロールします。
type Controller struct {
	registers ymf.Registers
	libraries []*smaf.VM5VoiceLib

	midiChannelStates [16]*midiChannelState
	slots             [ymfdata.ChannelCount]*slot
}

// NewController は、新しい Controller を作成します。
func NewController(registers ymf.Registers, libraries []*smaf.VM5VoiceLib) *Controller {
	ctrl := &Controller{
		registers: registers,
		libraries: libraries,
	}
	for i := range ctrl.slots {
		ctrl.slots[i] = &slot{}
	}
	for i := range ctrl.midiChannelStates {
		ctrl.midiChannelStates[i] = &midiChannelState{}
	}
	return ctrl
}

// NoteOn は、MIDIノートオン受信時の音源の振る舞いを再現します。
func (ctrl *Controller) NoteOn(ch, note, velocity int) {
	if velocity == 0 {
		ctrl.NoteOff(ch, note)
		return
	}

	instr, ok := ctrl.getInstrument(ch, note)
	if !ok {
		// TODO: warning
		return
	}

	if instr.VoiceType != smaf.VoiceType_FM {
		fmt.Printf("unsupported voice type: @%d-%d-%d note=%d type=%s\n", instr.BankMsb, instr.BankLsb, instr.Pc, note, instr.VoiceType)
		return
	}

	slotID := ctrl.findFreeSlot(ch, note)
	if 0 <= slotID {
		ctrl.occupySlot(slotID, ch, note, velocity, instr)
	} else {
		// TODO: warning
	}
}

// NoteOff は、MIDIノートオフ受信時の音源の振る舞いを再現します。
func (ctrl *Controller) NoteOff(ch, note int) {
	sus := ctrl.midiChannelStates[ch].sustain
	for slotID, slot := range ctrl.slots {
		if slot.midiChannel == ch && slot.note == note {
			if sus < 0x40 {
				ctrl.releaseSlot(slotID, false)
			} else {
				slot.flags |= flagSustain
			}
		}
	}
}

// ControlChange は、MIDIコントロールチェンジ受信時の音源の振る舞いを再現します。
func (ctrl *Controller) ControlChange(midich, cc, value int) {
	switch cc {
	case ccBankMSB:
		ctrl.midiChannelStates[midich].bankMSB = uint8(value)
	case ccBankLSB:
		ctrl.midiChannelStates[midich].bankLSB = uint8(value)
	case ccModulation:
		ctrl.midiChannelStates[midich].modulation = uint8(value)
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				flags := slot.flags
				slot.time = time.Now()
				if modThresh <= value {
					slot.flags |= flagVibrato
					if slot.flags != flags {
						ctrl.writeModulation(i, slot.instrument, true)
					}
				} else {
					slot.flags &= ^flagVibrato
					if slot.flags != flags {
						ctrl.writeModulation(i, slot.instrument, false)
					}
				}
			}
		}

	case ccVolume: // change volume
		ctrl.midiChannelStates[midich].volume = uint8(value)
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				slot.time = time.Now()
				ctrl.registers.WriteChannel(i, ymf.VOLUME, value)
			}
		}

	case ccExpression: // change expression
		ctrl.midiChannelStates[midich].expression = uint8(value)
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				slot.time = time.Now()
				ctrl.registers.WriteChannel(i, ymf.EXPRESSION, value)
			}
		}

	case ccPan: // change pan (balance)
		ctrl.midiChannelStates[midich].pan = uint8(value)
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				slot.time = time.Now()
				ctrl.registers.WriteChannel(i, ymf.CHPAN, value)
			}
		}

	case ccSustainPedal: // change sustain pedal (hold)
		ctrl.midiChannelStates[midich].sustain = uint8(value)
		if value < 0x40 {
			ctrl.releaseSustain(midich)
		}

	case ccNotesOff: // turn off all notes that are not sustained
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				if ctrl.midiChannelStates[midich].sustain < 0x40 {
					ctrl.releaseSlot(i, false)
				} else {
					slot.flags |= flagSustain
				}
			}
		}

	case ccSoundsOff: // release all notes for this channel
		for i, slot := range ctrl.slots {
			if slot.midiChannel == midich {
				ctrl.releaseSlot(i, false)
			}
		}

	case ccRPNHi:
		ctrl.midiChannelStates[midich].rpn = (ctrl.midiChannelStates[midich].rpn & 0x007f) | (uint16(value) << 7)

	case ccRPNLo:
		ctrl.midiChannelStates[midich].rpn = (ctrl.midiChannelStates[midich].rpn & 0x3f80) | uint16(value)

	case ccNRPNLo, ccNRPNHi:
		ctrl.midiChannelStates[midich].rpn = 0x3fff

	case ccDataEntryHi:
		if ctrl.midiChannelStates[midich].rpn == 0 {
			ctrl.midiChannelStates[midich].pitchSens = uint16(value)*100 + (ctrl.midiChannelStates[midich].pitchSens % 100)
		}

	case ccDataEntryLo:
		if ctrl.midiChannelStates[midich].rpn == 0 {
			ctrl.midiChannelStates[midich].pitchSens = uint16(value) + uint16(ctrl.midiChannelStates[midich].pitchSens/100)*100
		}
	}
}

// ProgramChange は、MIDIプログラムチェンジ受信時の音源の振る舞いを再現します。
func (ctrl *Controller) ProgramChange(midich, pc int) {
	ctrl.midiChannelStates[midich].pc = uint8(pc)
}

// PitchBend は、MIDIピッチベンド受信時の音源の振る舞いを再現します。
func (ctrl *Controller) PitchBend(midich, l, h int) {
	pitch := h*128 + l - 8192
	pitch = int(float64(pitch)*float64(ctrl.midiChannelStates[midich].pitchSens)/(200*128) + 64)
	ctrl.midiChannelStates[midich].pitch = int8(pitch)
	for i, slot := range ctrl.slots {
		if slot.midiChannel == midich {
			slot.time = time.Now()
			slot.pitch = slot.finetune + pitch
			ctrl.writeFrequency(i, slot.realnote, slot.pitch, true)
		}
	}
}

// Reset は、音源の状態をリセットします。
func (ctrl *Controller) Reset() {
	for _, slot := range ctrl.slots {
		slot.midiChannel = -1
		slot.note = 0
		slot.flags = 0
		slot.realnote = 0
		slot.finetune = 0
		slot.pitch = 0
		slot.velocity = 0
		slot.instrument = nil
		slot.time = time.Time{}
	}
	for _, state := range ctrl.midiChannelStates {
		state.volume = 100
		state.pan = 64
	}
	ctrl.ymfShutup()
	ctrl.releaseAllSlots()
	ctrl.resetAllMIDIChannels()
}

func (ctrl *Controller) writeModulation(slot int, instr *smaf.VM35VoicePC, state bool) {
	fmvoice := instr.FmVoice

	// TODO: モジュレータではevbだけを見る(stateは無視)？
	o := fmvoice.Operators
	ctrl.ymfWriteSlotEachOps(
		ymf.EVB,
		slot,
		bool2int(o[0].Evb || state),
		bool2int(o[1].Evb || state),
		bool2int(o[2].Evb || state),
		bool2int(o[3].Evb || state),
	)
}

func (ctrl *Controller) occupySlot(slotID, midich, note, velocity int, instr *smaf.VM35VoicePC) {
	state := ctrl.midiChannelStates[midich]
	slot := ctrl.slots[slotID]
	slot.midiChannel = midich
	slot.note = note
	slot.flags = 0
	if modThresh <= state.modulation {
		slot.flags |= flagVibrato
	}
	slot.time = time.Now()

	slot.velocity = velocity
	slot.finetune = 0
	if instr.DrumNote != 0 {
		note = int(instr.FmVoice.DrumKey)
	}
	slot.pitch = slot.finetune + int(state.pitch)
	slot.instrument = instr
	if instr.DrumNote == 0 {
		// for note < 0 {
		// 	note += 12
		// }
		// for 127 < note {
		// 	note -= 12
		// }
	}
	note += 2 - 12
	slot.realnote = note

	ctrl.ymfWriteInstrument(slotID, instr)
	ctrl.writeModulation(slotID, instr, slot.flags&flagVibrato != 0)
	ctrl.registers.WriteChannel(slotID, ymf.CHPAN, int(ctrl.midiChannelStates[midich].pan))
	ctrl.registers.WriteChannel(slotID, ymf.VOLUME, int(ctrl.midiChannelStates[midich].volume))
	ctrl.registers.WriteChannel(slotID, ymf.EXPRESSION, int(ctrl.midiChannelStates[midich].expression))
	ctrl.ymfWriteVelocity(slotID, slot.velocity, instr)
	ctrl.writeFrequency(slotID, note, slot.pitch, true)
}

func (ctrl *Controller) releaseSlot(slotID int, killed bool) {
	slot := ctrl.slots[slotID]
	ctrl.writeFrequency(slotID, slot.realnote, slot.pitch, false)
	slot.midiChannel = -1
	slot.time = time.Now()
	slot.flags = flagFree
	if killed {
		ctrl.ymfWriteSlotAllOps(ymf.SL, slotID, 0)
		ctrl.ymfWriteSlotAllOps(ymf.RR, slotID, 15) // release rate - fastest
		ctrl.ymfWriteSlotAllOps(ymf.KSL, slotID, 0)
		ctrl.ymfWriteSlotAllOps(ymf.TL, slotID, 0x3f) // no volume
	}
}

func (ctrl *Controller) releaseSustain(midich int) {
	for i, slot := range ctrl.slots {
		if slot.midiChannel == midich && slot.flags&flagSustain != 0 {
			ctrl.releaseSlot(i, false)
		}
	}
}

func (ctrl *Controller) findFreeSlot(midich, note int) int {
	for i := 0; i < len(ctrl.slots); i++ {
		if ctrl.slots[i].flags&flagFree != 0 {
			return i
		}
	}

	oldest := -1
	oldesttime := time.Now()

	// find some 2nd-voice channel and determine the oldest
	for i := 0; i < len(ctrl.slots); i++ {
		if ctrl.slots[i].time.Before(oldesttime) {
			oldesttime = ctrl.slots[i].time
			oldest = i
		}
	}

	// if possible, kill the oldest channel
	if 0 <= oldest {
		ctrl.releaseSlot(oldest, true)
		return oldest
	}

	// can't find any free channel
	return -1
}

func (ctrl *Controller) getInstrument(midich, note int) (*smaf.VM35VoicePC, bool) {
	// TODO: smaf825側で検索
	// TODO: ドラム音色
	s := ctrl.midiChannelStates[midich]
	for _, lib := range ctrl.libraries {
		for _, p := range lib.Programs {
			if !(p.Pc == uint32(s.pc) && p.BankLsb == uint32(s.bankLSB) && p.BankMsb == uint32(s.bankMSB)) {
				continue
			}
			if p.DrumNote != 0 && int(p.DrumNote) != note {
				continue
			}
			return p, true
		}
	}
	// fmt.Printf("voice not found: @%d-%d-%d note=%d\n", s.bankMSB, s.bankLSB, s.pc, note)

	// TODO: Remove
	if s.bankMSB == 125 && s.pc != 1 {
		s.pc = 1
		return ctrl.getInstrument(midich, note)
	}

	return ctrl.libraries[0].Programs[0], false
}

func (ctrl *Controller) resetMIDIChannel(midich int) {
	ctrl.midiChannelStates[midich].volume = 100
	ctrl.midiChannelStates[midich].expression = 127
	ctrl.midiChannelStates[midich].sustain = 0
	ctrl.midiChannelStates[midich].pitch = 64
	ctrl.midiChannelStates[midich].rpn = 0x3fff
	ctrl.midiChannelStates[midich].pitchSens = 200
}

func (ctrl *Controller) resetAllMIDIChannels() {
	for i := range ctrl.midiChannelStates {
		ctrl.resetMIDIChannel(i)
	}
}

func (ctrl *Controller) releaseAllSlots() {
	for i := range ctrl.slots {
		if ctrl.slots[i].flags&flagFree == 0 {
			ctrl.releaseSlot(i, true)
		}
	}
}

func (ctrl *Controller) ymfWriteSlotAllOps(regbase ymf.OpRegister, slotID, data int) {
	ctrl.registers.WriteOperator(slotID, 0, regbase, data)
	ctrl.registers.WriteOperator(slotID, 1, regbase, data)
	ctrl.registers.WriteOperator(slotID, 2, regbase, data)
	ctrl.registers.WriteOperator(slotID, 3, regbase, data)
}

func (ctrl *Controller) ymfWriteSlotEachOps(regbase ymf.OpRegister, slotID, data1, data2, data3, data4 int) {
	ctrl.registers.WriteOperator(slotID, 0, regbase, data1)
	ctrl.registers.WriteOperator(slotID, 1, regbase, data2)
	ctrl.registers.WriteOperator(slotID, 2, regbase, data3)
	ctrl.registers.WriteOperator(slotID, 3, regbase, data4)
}

func (ctrl *Controller) writeFrequency(slotID, note, pitch int, keyon bool) {
	n := float64(note-ymfdata.A3Note) + float64(pitch-64)/32.0
	freq := ymfdata.A3Freq * math.Pow(2.0, n/12.0)

	block := note / 12
	if 7 < block {
		block = 7
	}

	fnum := int(freq*ymfdata.FNUMCoef) >> uint(block-1)
	if fnum < 0 {
		fnum = 0
	} else {
		for 1024 < fnum {
			block++
			fnum >>= 1
		}
	}
	if block < 0 {
		block = 0
	} else if 7 < block {
		block = 7
	}

	ctrl.registers.WriteChannel(slotID, ymf.FNUM, fnum)
	ctrl.registers.WriteChannel(slotID, ymf.BLOCK, block)
	k := 0
	if keyon {
		k = 1
	}
	ctrl.registers.WriteChannel(slotID, ymf.KON, k)
}

func ymfConvertVelocity(data, velocity int) int {
	r := int(velocitytable[velocity])
	return 0x3f - ((0x3f - data) * r >> 7)
}

func (ctrl *Controller) ymfWriteVelocity(slotID, velocity int, instr *smaf.VM35VoicePC) {
	for i, op := range instr.FmVoice.Operators {
		tlModulator := int(op.Tl)
		tlCarrier := ymfConvertVelocity(tlModulator, velocity)
		ctrl.registers.WriteTL(slotID, i, tlCarrier, tlModulator)
	}
}

func bool2int(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (ctrl *Controller) ymfWriteInstrument(slotID int, instr *smaf.VM35VoicePC) {
	ctrl.ymfWriteSlotAllOps(ymf.TL, slotID, 0x3f) // no volume

	for i, op := range instr.FmVoice.Operators {
		ctrl.registers.WriteOperator(slotID, i, ymf.EAM, bool2int(op.Eam))
		ctrl.registers.WriteOperator(slotID, i, ymf.EVB, bool2int(op.Evb))
		ctrl.registers.WriteOperator(slotID, i, ymf.DAM, int(op.Dam))
		ctrl.registers.WriteOperator(slotID, i, ymf.DVB, int(op.Dvb))
		ctrl.registers.WriteOperator(slotID, i, ymf.DT, int(op.Dt))
		ctrl.registers.WriteOperator(slotID, i, ymf.KSL, int(op.Ksl))
		ctrl.registers.WriteOperator(slotID, i, ymf.KSR, bool2int(op.Ksr))
		ctrl.registers.WriteOperator(slotID, i, ymf.WS, int(op.Ws))
		ctrl.registers.WriteOperator(slotID, i, ymf.MULT, int(op.Multi))
		ctrl.registers.WriteOperator(slotID, i, ymf.FB, int(op.Fb))
		ctrl.registers.WriteOperator(slotID, i, ymf.AR, int(op.Ar))
		ctrl.registers.WriteOperator(slotID, i, ymf.DR, int(op.Dr))
		ctrl.registers.WriteOperator(slotID, i, ymf.SL, int(op.Sl))
		ctrl.registers.WriteOperator(slotID, i, ymf.SR, int(op.Sr))
		ctrl.registers.WriteOperator(slotID, i, ymf.RR, int(op.Rr))
		ctrl.registers.WriteOperator(slotID, i, ymf.TL, int(op.Tl))
		ctrl.registers.WriteOperator(slotID, i, ymf.XOF, bool2int(op.Xof))
	}

	ctrl.registers.WriteChannel(slotID, ymf.ALG, int(instr.FmVoice.Alg))
	ctrl.registers.WriteChannel(slotID, ymf.LFO, int(instr.FmVoice.Lfo))
	ctrl.registers.WriteChannel(slotID, ymf.PANPOT, int(instr.FmVoice.Panpot))
	ctrl.registers.WriteChannel(slotID, ymf.BO, int(instr.FmVoice.Bo))
}

func (ctrl *Controller) ymfShutup() {
	for i := range ctrl.slots {
		ctrl.ymfWriteSlotAllOps(ymf.KSL, i, 0)
		ctrl.ymfWriteSlotAllOps(ymf.TL, i, 0x3f)   // turn off volume
		ctrl.ymfWriteSlotAllOps(ymf.AR, i, 15)     // the fastest attack,
		ctrl.ymfWriteSlotAllOps(ymf.DR, i, 15)     // decay
		ctrl.ymfWriteSlotAllOps(ymf.SL, i, 0)      //
		ctrl.ymfWriteSlotAllOps(ymf.RR, i, 15)     // ... and release
		ctrl.registers.WriteChannel(i, ymf.KON, 0) // KEY-OFF
	}
}

var velocitytable = [...]uint8{
	0, 1, 3, 5, 6, 8, 10, 11,
	13, 14, 16, 17, 19, 20, 22, 23,
	25, 26, 27, 29, 30, 32, 33, 34,
	36, 37, 39, 41, 43, 45, 47, 49,
	50, 52, 54, 55, 57, 59, 60, 61,
	63, 64, 66, 67, 68, 69, 71, 72,
	73, 74, 75, 76, 77, 79, 80, 81,
	82, 83, 84, 84, 85, 86, 87, 88,
	89, 90, 91, 92, 92, 93, 94, 95,
	96, 96, 97, 98, 99, 99, 100, 101,
	101, 102, 103, 103, 104, 105, 105, 106,
	107, 107, 108, 109, 109, 110, 110, 111,
	112, 112, 113, 113, 114, 114, 115, 115,
	116, 117, 117, 118, 118, 119, 119, 120,
	120, 121, 121, 122, 122, 123, 123, 123,
	124, 124, 125, 125, 126, 126, 127, 127,
}
