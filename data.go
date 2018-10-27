package computrainer

// DataType indicates what type of metric is being reported in a message
type DataType uint8

// DataType values
const (
	DataNone      DataType = 0x00
	DataSpeed              = 0x01
	DataPower              = 0x02
	DataHeartRate          = 0x03
	DataCadence            = 0x06
	DataRRC                = 0x09
	DataSensor             = 0x0b
)

// Button bit mappings
const (
	ButtonReset = 1 << iota
	ButtonF1
	ButtonF3
	ButtonPlus
	ButtonF2
	ButtonMinus
	ButtonSpinScan
	ButtonNone
)

// Message is a representation of a message published by the CompuTrainer
type Message struct {
	Buttons uint8
	SS1     uint16
	SS2     uint16
	SS3     uint16
	Type    DataType
	Value   uint16
}

// ParseMessage decodes messages published by the CompuTrainer
func ParseMessage(msg []byte) Message {
	s1 := msg[0] // ss data
	s2 := msg[1] // ss data
	s3 := msg[2] // ss data
	bt := msg[3] // button data
	b1 := msg[4] // message and value
	b2 := msg[5] // value
	b3 := msg[6] // the dregs (sync, z and lsb for all the others)

	dataType := DataType((b1 & 120) >> 3)
	value8 := uint16((b2&^128)<<1 | (b3 & 1))

	var value uint16
	switch dataType {
	case DataCadence, DataHeartRate:
		value = value8
	case DataRRC:
		if (b1 & 4) > 0 {
			value = value8 | uint16(b1&3)<<9 | uint16(b3&2)<<7
		} else {
			value = 0
		}
	case DataPower, DataSpeed:
		value = value8 | uint16(b1&7)<<9 | uint16(b3&2)<<7
	default:
		dataType = DataNone
	}

	return Message{
		Buttons: bt<<1 | (b3&4)>>2,
		SS1:     uint16(s1)<<1 | uint16(b3&32)>>5,
		SS2:     uint16(s2)<<1 | uint16(b3&16)>>4,
		SS3:     uint16(s3)<<1 | uint16(b3&8)>>3,
		Type:    dataType,
		Value:   value,
	}
}
