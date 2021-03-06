package sim

import (
	"fmt"
	"math"
	"strings"

	"github.com/but80/fmfm.core/ymf/ymfdata"
)

const noModulator = 0

/*

==================================================
MA-5

ALG=0
  (FB)1 -> 2 -> OUT

ALG=1
  (FB)1 -> | -> OUT
      2 -> |

ALG=2
  (FB)1 -> | -> OUT
      2 -> |
  (FB)3 -> |
      4 -> |

ALG=3
  (FB)1 ------> | -> 4 -> OUT
      2 -> 3 -> |

ALG=4
  (FB)1 -> 2 -> 3 -> 4 -> OUT

ALG=5
  (FB)1 -> 2 -> | -> OUT
  (FB)3 -> 4 -> |

ALG=6
  (FB)1 -----------> | -> OUT
      2 -> 3 -> 4 -> |

ALG=7
  (FB)1 ------> | -> OUT
      2 -> 3 -> |
      4 ------> |

==================================================
OPL3

| ADDR | 7 | 6 | 5 | 4 | 3 | 2 | 1 | 0 |
|C0..C8|CHD|CHC|CHB|CHA|    F B    |CNT|

===== 2 operators mode =====

CNT = 0
  (FB)OP1 -> OP2 -> OUT

CNT = 1
  (FB)OP1 -> |
      OP2 -> | -> OUT

===== 4 operators mode =====

|Channel No.|  1  |  2  |  3  |  4  |  5  |  6  |
|CNT Address|C0,C3|C1,C4|C2,C5|C0,C3|C1,C4|C2,C5|
|    A1     |       L         |        H        |

CNT(Cn) = 0, CNT(Cn+3) = 0
  (FB)OP1 -> OP2 -> OP3 -> OP4 -> OUT

CNT(Cn) = 0, CNT(Cn+3) = 1
  (FB)OP1 -> OP2 -> |
      OP3 -> OP4 -> | -> OUT

CNT(Cn) = 1, CNT(Cn+3) = 0
  (FB)OP1 ---------------> |
      OP2 -> OP3 -> OP4 -> | -> OUT

CNT(Cn) = 1, CNT(Cn+3) = 1
  (FB)OP1 --------> |
      OP2 -> OP3 -> |
      OP4 --------> | -> OUT
*/

// Channel は、音源のチャンネルです。
type Channel struct {
	channelID     int
	midiChannelID int

	chip       *Chip
	fnum       int
	kon        int
	block      int
	alg        int
	panpot     int
	chpan      int
	volume     int
	expression int
	velocity   int
	bo         int

	feedbackBlendPrev float64
	feedbackBlendCurr float64
	feedback1Prev     float64
	feedback1Curr     float64
	feedback3Prev     float64
	feedback3Curr     float64
	feedbackOut1      float64
	feedbackOut3      float64
	attenuationCoef   float64
	modIndexFrac64    ymfdata.Frac64
	lfoFrequency      ymfdata.Frac64
	panCoefL          float64
	panCoefR          float64

	operators [4]*operator
}

func newChannel(channelID int, chip *Chip) *Channel {
	ch := &Channel{
		chip:      chip,
		channelID: channelID,
	}

	// 48000Hz:     |prev|curr|
	// 44100Hz: | prev | curr |
	ch.feedbackBlendCurr = .5 * ymfdata.SampleRate / chip.sampleRate
	if 1.0 < ch.feedbackBlendCurr {
		ch.feedbackBlendCurr = 1.0
	}
	ch.feedbackBlendPrev = 1.0 - ch.feedbackBlendCurr

	for i := range ch.operators {
		ch.operators[i] = newOperator(channelID, i, chip)
	}

	ch.resetAll()
	return ch
}

func (ch *Channel) reset() {
	// TODO: モジュレーションは発音ごとにリセットされるのか？
	ch.modIndexFrac64 = 0
	ch.feedback1Prev = .0
	ch.feedback1Curr = .0
	ch.feedback3Prev = .0
	ch.feedback3Curr = .0
	ch.feedbackOut1 = .0
	ch.feedbackOut3 = .0
	for _, op := range ch.operators {
		op.phaseGenerator.reset()
		op.envelopeGenerator.reset()
	}
}

func (ch *Channel) resetAll() {
	ch.midiChannelID = -1
	ch.fnum = 0
	ch.kon = 0
	ch.block = 0
	ch.alg = 0
	ch.panpot = 15
	ch.chpan = 64
	ch.volume = 100
	ch.expression = 127
	ch.velocity = 0
	ch.bo = 1
	ch.setLFO(0)
	ch.updatePanCoef()
	ch.updateAttenuation()
	for _, op := range ch.operators {
		op.resetAll()
	}
}

func (ch *Channel) isOff() bool {
	for i, op := range ch.operators {
		if !ymfdata.CarrierMatrix[ch.alg][i] {
			continue
		}
		if op.envelopeGenerator.stage != stageOff {
			return false
		}
	}
	return true
}

func (ch *Channel) currentLevel() float64 {
	result := .0
	for i, op := range ch.operators {
		if ymfdata.CarrierMatrix[ch.alg][i] {
			eg := op.envelopeGenerator
			v := eg.currentLevel * eg.kslTlCoef
			if result < v {
				result = v
			}
		}
	}
	return result
}

func (ch *Channel) dump() string {
	lv := int((96.0 + math.Log10(ch.currentLevel())*20.0) / 8.0)
	lvstr := strings.Repeat("|", lv)
	result := fmt.Sprintf(
		"[%02d] midi=%02d alg=%d pan=%03d+%03d vol=%03d exp=%03d vel=%03d freq=%03d+%d-%d modidx=%04d %s\n",
		ch.channelID,
		ch.midiChannelID,
		ch.alg,
		ch.panpot,
		ch.chpan,
		ch.volume,
		ch.expression,
		ch.velocity,
		// ch.attenuationCoef,
		ch.fnum,
		ch.block,
		ch.bo,
		ch.modIndexFrac64>>ymfdata.ModTableIndexShift,
		// ch.lfoFrequency,
		// ch.panCoefL,
		// ch.panCoefR,
		lvstr,
	)
	for _, op := range ch.operators {
		result += "  " + op.dump() + "\n"
	}
	return result
}

func (ch *Channel) setKON(v int) {
	if v == 0 {
		ch.keyOff()
		if ch.isOff() {
			ch.resetAll()
		}
	} else {
		ch.keyOn()
	}
}

func (ch *Channel) keyOn() {
	if ch.kon != 0 {
		return
	}
	for _, op := range ch.operators {
		op.keyOn()
	}
	ch.kon = 1
}

func (ch *Channel) keyOff() {
	if ch.kon == 0 {
		return
	}
	for _, op := range ch.operators {
		op.keyOff()
	}
	ch.kon = 0
}

func (ch *Channel) setBLOCK(v int) {
	ch.block = v
	ch.updateFrequency()
}

func (ch *Channel) setFNUM(v int) {
	ch.fnum = v
	ch.updateFrequency()
}

func (ch *Channel) setALG(v int) {
	if ch.alg != v {
		ch.reset()
	}
	ch.alg = v
	ch.feedback1Prev = 0
	ch.feedback1Curr = 0
	ch.feedback3Prev = 0
	ch.feedback3Curr = 0
	for i, op := range ch.operators {
		op.isModulator = ymfdata.ModulatorMatrix[ch.alg][i]
	}
}

func (ch *Channel) setLFO(v int) {
	ch.lfoFrequency = ymfdata.LFOFrequency[v]
}

func (ch *Channel) setPANPOT(v int) {
	ch.panpot = v
	ch.updatePanCoef()
}

func (ch *Channel) setCHPAN(v int) {
	ch.chpan = v
	ch.updatePanCoef()
}

func (ch *Channel) updatePanCoef() {
	pan := ch.chpan + (ch.panpot-15)*4
	if pan < 0 {
		pan = 0
	} else if 127 < pan {
		pan = 127
	}
	ch.panCoefL = ymfdata.PanTable[pan][0]
	ch.panCoefR = ymfdata.PanTable[pan][1]
}

func (ch *Channel) setVOLUME(v int) {
	ch.volume = v
	ch.updateAttenuation()
}

func (ch *Channel) setEXPRESSION(v int) {
	ch.expression = v
	ch.updateAttenuation()
}

func (ch *Channel) setVELOCITY(v int) {
	ch.velocity = v
	ch.updateAttenuation()
}

func (ch *Channel) updateAttenuation() {
	ch.attenuationCoef = ymfdata.VolumeTable[ch.volume>>2] * ymfdata.VolumeTable[ch.expression>>2] * ymfdata.VolumeTable[ch.velocity>>2]
}

func (ch *Channel) setBO(v int) {
	ch.bo = v
	ch.updateFrequency()
}

func (ch *Channel) next() (float64, float64) {
	var result float64
	var op1out float64
	var op2out float64
	var op3out float64
	var op4out float64

	op1 := ch.operators[0]
	op2 := ch.operators[1]
	op3 := ch.operators[2]
	op4 := ch.operators[3]

	modIndex := int(ch.modIndexFrac64 >> ymfdata.ModTableIndexShift)
	ch.modIndexFrac64 += ch.lfoFrequency

	switch ch.alg {

	case 0:
		// (FB)1 -> 2 -> OUT
		if op2.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)

		result = op2.next(modIndex, op1out*ymfdata.ModulatorMultiplier)

	case 1:
		// (FB)1 -> | -> OUT
		//     2 -> |
		if op1.envelopeGenerator.stage == stageOff && op2.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, noModulator)

		result = op1out + op2out

	case 2:
		// (FB)1 -> | -> OUT
		//     2 -> |
		// (FB)3 -> |
		//     4 -> |
		if op1.envelopeGenerator.stage == stageOff &&
			op2.envelopeGenerator.stage == stageOff &&
			op3.envelopeGenerator.stage == stageOff &&
			op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, noModulator)
		op3out = op3.next(modIndex, ch.feedbackOut3)
		op4out = op4.next(modIndex, noModulator)

		result = op1out + op2out + op3out + op4out

	case 3:
		// (FB)OP1 --------> | -> OP4 -> OUT
		//     OP2 -> OP3 -> |
		if op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, noModulator)
		op3out = op3.next(modIndex, op2out*ymfdata.ModulatorMultiplier)

		result = op4.next(modIndex, (op1out+op3out)*ymfdata.ModulatorMultiplier)

	case 4:
		// (FB)OP1 -> OP2 -> OP3 -> OP4 -> OUT
		if op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, op1out*ymfdata.ModulatorMultiplier)
		op3out = op3.next(modIndex, op2out*ymfdata.ModulatorMultiplier)

		result = op4.next(modIndex, op3out*ymfdata.ModulatorMultiplier)

	case 5:
		// (FB)OP1 -> OP2 -> | -> OUT
		// (FB)OP3 -> OP4 -> |
		if op2.envelopeGenerator.stage == stageOff && op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, op1out*ymfdata.ModulatorMultiplier)

		op3out = op3.next(modIndex, ch.feedbackOut3)
		op4out = op4.next(modIndex, op3out*ymfdata.ModulatorMultiplier)

		result = op2out + op4out

	case 6:
		// (FB)OP1 ---------------> | -> OUT
		//     OP2 -> OP3 -> OP4 -> |
		if op1.envelopeGenerator.stage == stageOff && op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, noModulator)
		op3out = op3.next(modIndex, op2out*ymfdata.ModulatorMultiplier)
		op4out = op4.next(modIndex, op3out*ymfdata.ModulatorMultiplier)

		result = op1out + op4out

	case 7:
		// (FB)OP1 --------> | -> OUT
		//     OP2 -> OP3 -> |
		//     OP4 --------> |
		if op1.envelopeGenerator.stage == stageOff &&
			op3.envelopeGenerator.stage == stageOff &&
			op4.envelopeGenerator.stage == stageOff {
			return 0, 0
		}

		op1out = op1.next(modIndex, ch.feedbackOut1)
		op2out = op2.next(modIndex, noModulator)
		op3out = op3.next(modIndex, op2out*ymfdata.ModulatorMultiplier)
		op4out = op4.next(modIndex, noModulator)

		result = op1out + op3out + op4out
	}

	if op1.feedbackCoef != .0 {
		ch.feedback1Prev = ch.feedback1Curr
		ch.feedback1Curr = op1out * op1.feedbackCoef
		ch.feedbackOut1 = ch.feedback1Prev*ch.feedbackBlendPrev + ch.feedback1Curr*ch.feedbackBlendCurr
	}

	if op3.feedbackCoef != .0 {
		ch.feedback3Prev = ch.feedback3Curr
		ch.feedback3Curr = op3out * op3.feedbackCoef
		ch.feedbackOut3 = ch.feedback3Prev*ch.feedbackBlendPrev + ch.feedback3Curr*ch.feedbackBlendCurr
	}

	result *= ch.attenuationCoef
	return result * ch.panCoefL, result * ch.panCoefR
}

func (ch *Channel) updateFrequency() {
	for _, op := range ch.operators {
		op.setFrequency(ch.fnum, ch.block, ch.bo)
	}
}
