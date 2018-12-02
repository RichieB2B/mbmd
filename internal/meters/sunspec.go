package meters

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
)

const (
	// MODBUS protocol address (base 0)
	sunspecBase         = 40000
	sunspecID           = 1
	sunspecModelID      = 3
	sunspecManufacturer = 5
	sunspecModel        = 21
	sunspecVersion      = 45
	sunspecSerial       = 53

	sunsSignature = 0x53756e53 // SunS
)

type SunSpecDeviceDescriptor struct {
	Manufacturer string
	Model        string
	Version      string
	Serial       string
}

// RTUUint16ToFloat64WithNaN converts 16 bit unsigned integer readings
// If byte sequence is 0xffff, NaN is returned for compatibility with SunSpec/SE 1-phase inverters
func RTUUint16ToFloat64WithNaN(b []byte) float64 {
	u := binary.BigEndian.Uint16(b)
	if u == 0xffff {
		return math.NaN()
	}
	return float64(u)
}

type SunSpecCore struct {
	MeasurementMapping
}

func (p *SunSpecCore) GetSunSpecCommonBlock() Operation {
	// must return 0x53756e53 = SunS
	return Operation{
		FuncCode: ReadHoldingReg,
		OpCode:   sunspecBase, // adjust according to docs
		ReadLen:  sunspecSerial,
		// IEC61850: iec,
	}
}

func (p *SunSpecCore) DecodeSunSpecCommonBlock(b []byte) (SunSpecDeviceDescriptor, error) {
	// log.Printf("%0 x", b)
	res := SunSpecDeviceDescriptor{}

	if len(b) < sunspecSerial+2*16 {
		return res, errors.New("Could not read SunSpec device descriptor")
	}

	u := binary.BigEndian.Uint32(b[sunspecID-1:])
	if u != sunsSignature {
		return res, errors.New("Invalid SunSpec device signature")
	}

	res.Manufacturer = p.stringDecode(b, sunspecManufacturer, 16)
	res.Model = p.stringDecode(b, sunspecModel, 16)
	res.Version = p.stringDecode(b, sunspecVersion, 8)
	res.Serial = p.stringDecode(b, sunspecSerial, 16)

	return res, nil
}

func (p *SunSpecCore) stringDecode(b []byte, reg int, len int) string {
	start := 2 * (reg - 1)
	end := 2 * (reg + len - 1)
	// trim space and null
	return strings.TrimRight(string(b[start:end-1]), " \x00")
}

func (p *SunSpecCore) snip(iec Measurement, readlen uint16) Operation {
	return Operation{
		FuncCode: ReadHoldingReg,
		OpCode:   sunspecBase + p.Opcode(iec) - 1, // adjust according to docs
		ReadLen:  readlen,
		IEC61850: iec,
	}
}

func (p *SunSpecCore) snip16uint(iec Measurement, scaler ...float64) Operation {
	snip := p.snip(iec, 1)

	snip.Transform = RTUUint16ToFloat64 // default conversion
	if len(scaler) > 0 {
		snip.Transform = MakeRTUScaledUint16ToFloat64(scaler[0])
	}

	return snip
}

func (p *SunSpecCore) snip16int(iec Measurement, scaler ...float64) Operation {
	snip := p.snip(iec, 1)

	snip.Transform = RTUInt16ToFloat64 // default conversion
	if len(scaler) > 0 {
		snip.Transform = MakeRTUScaledInt16ToFloat64(scaler[0])
	}

	return snip
}

func (p *SunSpecCore) snip32(iec Measurement, scaler ...float64) Operation {
	snip := p.snip(iec, 2)

	snip.Transform = RTUUint32ToFloat64 // default conversion
	if len(scaler) > 0 {
		snip.Transform = MakeRTUScaledUint32ToFloat64(scaler[0])
	}

	return snip
}

func (p *SunSpecCore) minMax(iec ...Measurement) (uint16, uint16) {
	var min = uint16(0xFFFF)
	var max = uint16(0x0000)
	for _, i := range iec {
		op := p.Opcode(i)
		if op < min {
			min = op
		}
		if op > max {
			max = op
		}
	}
	return min, max
}

// create a block reading function the result of which is then split into measurements
func (p *SunSpecCore) scaleSnip16(splitter func(...Measurement) Splitter, iecs ...Measurement) Operation {
	min, max := p.minMax(iecs...)

	// read register block
	op := Operation{
		FuncCode: ReadHoldingReg,
		OpCode:   sunspecBase + min - 1, // adjust according to docs
		ReadLen:  max - min + 2,         // registers plus int16 scale factor
		IEC61850: Split,
		Splitter: splitter(iecs...),
	}

	return op
}

func (p *SunSpecCore) scaleSnip32(splitter func(...Measurement) Splitter, iecs ...Measurement) Operation {
	op := p.scaleSnip16(splitter, iecs...)
	op.ReadLen = (op.ReadLen-1)*2 + 1 // read 4 bytes instead of 2 plus trailing scale factor
	return op
}

func (p *SunSpecCore) mkSplitInt16(iecs ...Measurement) Splitter {
	return p.mkBlockSplitter(2, RTUInt16ToFloat64, iecs...)
}

func (p *SunSpecCore) mkSplitUint16(iecs ...Measurement) Splitter {
	return p.mkBlockSplitter(2, RTUUint16ToFloat64WithNaN, iecs...)
}

func (p *SunSpecCore) mkSplitUint32(iecs ...Measurement) Splitter {
	// use div 1000 for kWh conversion
	return p.mkBlockSplitter(4, MakeRTUScaledUint32ToFloat64(1000), iecs...)
}

func (p *SunSpecCore) mkBlockSplitter(dataSize uint16, valFunc func([]byte) float64, iecs ...Measurement) Splitter {
	min, _ := p.minMax(iecs...)
	return func(b []byte) []SplitResult {
		// get scaler from last entry in result block
		exp := int(int16(binary.BigEndian.Uint16(b[len(b)-2:]))) // last int16
		scaler := math.Pow10(exp)

		res := make([]SplitResult, 0, len(iecs))

		// split result block into individual readings
		for _, iec := range iecs {
			opcode := p.Opcode(iec)
			val := valFunc(b[dataSize*(opcode-min):]) // 2 bytes per uint16, 4 bytes per uint32

			// filter results of RTUUint16ToFloat64WithNaN
			if math.IsNaN(val) {
				continue
			}

			op := SplitResult{
				OpCode:   sunspecBase + opcode - 1,
				IEC61850: iec,
				Value:    scaler * val,
			}

			res = append(res, op)
		}

		return res
	}
}