package wsproto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Cabecera binaria de un frame de cámara. 24 bytes big-endian, seguidos del
// payload codificado:
//
//	offset  0  uint8   versión (=1)
//	offset  1  uint8   codificación
//	offset  2  uint16  flags (reservado, 0)
//	offset  4  uint64  frame_seq
//	offset 12  uint64  captured_at_us — reloj del CLIENTE
//	offset 20  uint32  longitud del payload
//
// Sobre captured_at_us: es el reloj del cliente y NO PUNTÚA. El gateway sella
// cada frame con su propio reloj al recibirlo y usa ese sello para todo lo
// que decide. El del cliente sólo sirve para alinear frames entre sí, y sólo
// si su deriva contra el reloj del servidor resulta plausible.
const (
	// FrameHeaderSize es el tamaño fijo de la cabecera.
	FrameHeaderSize = 24
	// FrameVersion es la versión soportada.
	FrameVersion = 1
)

// Errores de parseo de frames.
var (
	ErrFrameTooShort       = errors.New("wsproto: frame más corto que la cabecera")
	ErrFrameVersion        = errors.New("wsproto: versión de frame no soportada")
	ErrFrameLengthMismatch = errors.New("wsproto: la longitud declarada no coincide con el payload")
)

// Encoding es la codificación del payload de un frame.
type Encoding uint8

// Codificaciones.
const (
	EncodingUnspecified Encoding = 0
	EncodingJPEG        Encoding = 1
	EncodingWebP        Encoding = 2
	EncodingRGB24       Encoding = 3
	// EncodingSyntheticScene es material sintético del banco de pruebas: el
	// payload describe la escena en JSON en vez de traer píxeles. Sólo lo
	// entiende el analizador de pruebas.
	EncodingSyntheticScene Encoding = 0xF0
)

// String implementa fmt.Stringer.
func (e Encoding) String() string {
	switch e {
	case EncodingJPEG:
		return "jpeg"
	case EncodingWebP:
		return "webp"
	case EncodingRGB24:
		return "rgb24"
	case EncodingSyntheticScene:
		return "synthetic_scene"
	case EncodingUnspecified:
		return "unspecified"
	default:
		return fmt.Sprintf("encoding(%d)", uint8(e))
	}
}

// FrameHeader es la cabecera de un frame.
type FrameHeader struct {
	Version  uint8
	Encoding Encoding
	Flags    uint16
	Seq      uint64
	// CapturedAtUS es el instante de captura según el reloj del CLIENTE, en
	// microsegundos epoch. No es de fiar.
	CapturedAtUS int64
	PayloadLen   uint32
}

// ParseFrame separa la cabecera del payload.
//
// No copia: el payload apunta al buffer de entrada.
func ParseFrame(b []byte) (FrameHeader, []byte, error) {
	if len(b) < FrameHeaderSize {
		return FrameHeader{}, nil, fmt.Errorf("%w: %d bytes", ErrFrameTooShort, len(b))
	}
	h := FrameHeader{
		Version:      b[0],
		Encoding:     Encoding(b[1]),
		Flags:        binary.BigEndian.Uint16(b[2:4]),
		Seq:          binary.BigEndian.Uint64(b[4:12]),
		CapturedAtUS: int64(binary.BigEndian.Uint64(b[12:20])),
		PayloadLen:   binary.BigEndian.Uint32(b[20:24]),
	}
	if h.Version != FrameVersion {
		return FrameHeader{}, nil, fmt.Errorf("%w: %d", ErrFrameVersion, h.Version)
	}
	payload := b[FrameHeaderSize:]
	if int(h.PayloadLen) != len(payload) {
		return FrameHeader{}, nil, fmt.Errorf("%w: declara %d, hay %d",
			ErrFrameLengthMismatch, h.PayloadLen, len(payload))
	}
	return h, payload, nil
}

// AppendFrame serializa cabecera y payload sobre dst.
func AppendFrame(dst []byte, h FrameHeader, payload []byte) []byte {
	var hdr [FrameHeaderSize]byte
	hdr[0] = FrameVersion
	hdr[1] = byte(h.Encoding)
	binary.BigEndian.PutUint16(hdr[2:4], h.Flags)
	binary.BigEndian.PutUint64(hdr[4:12], h.Seq)
	binary.BigEndian.PutUint64(hdr[12:20], uint64(h.CapturedAtUS))
	binary.BigEndian.PutUint32(hdr[20:24], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}
