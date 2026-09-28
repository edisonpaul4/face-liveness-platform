package wsproto

import (
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	want := FrameHeader{
		Version:      FrameVersion,
		Encoding:     EncodingJPEG,
		Seq:          1234567890,
		CapturedAtUS: 1_755_000_000_000_000,
	}
	payload := []byte("no son píxeles pero sirven")

	raw := AppendFrame(nil, want, payload)
	if len(raw) != FrameHeaderSize+len(payload) {
		t.Fatalf("el frame ocupa %d bytes, se esperaban %d", len(raw), FrameHeaderSize+len(payload))
	}

	got, gotPayload, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("ParseFrame: %v", err)
	}
	if got.Encoding != want.Encoding || got.Seq != want.Seq || got.CapturedAtUS != want.CapturedAtUS {
		t.Errorf("cabecera %+v, se esperaba %+v", got, want)
	}
	if got.PayloadLen != uint32(len(payload)) {
		t.Errorf("PayloadLen = %d, se esperaba %d", got.PayloadLen, len(payload))
	}
	if string(gotPayload) != string(payload) {
		t.Errorf("payload = %q", gotPayload)
	}
}

func TestParseFrameRejectsGarbage(t *testing.T) {
	valid := AppendFrame(nil, FrameHeader{Encoding: EncodingJPEG, Seq: 1}, []byte("hola"))

	t.Run("demasiado corto", func(t *testing.T) {
		if _, _, err := ParseFrame(valid[:10]); !errors.Is(err, ErrFrameTooShort) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("versión desconocida", func(t *testing.T) {
		bad := append([]byte(nil), valid...)
		bad[0] = 9
		if _, _, err := ParseFrame(bad); !errors.Is(err, ErrFrameVersion) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("longitud que no cuadra", func(t *testing.T) {
		// Un cliente que declara más payload del que manda no puede hacer
		// que el servidor lea de más.
		bad := append([]byte(nil), valid...)
		bad[23] = 200
		if _, _, err := ParseFrame(bad); !errors.Is(err, ErrFrameLengthMismatch) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("sin payload", func(t *testing.T) {
		empty := AppendFrame(nil, FrameHeader{Encoding: EncodingJPEG, Seq: 1}, nil)
		h, payload, err := ParseFrame(empty)
		if err != nil {
			t.Fatalf("un frame sin payload es válido: %v", err)
		}
		if h.PayloadLen != 0 || len(payload) != 0 {
			t.Errorf("PayloadLen = %d, payload = %d bytes", h.PayloadLen, len(payload))
		}
	})
}

func TestEncodingString(t *testing.T) {
	cases := map[Encoding]string{
		EncodingUnspecified:    "unspecified",
		EncodingJPEG:           "jpeg",
		EncodingWebP:           "webp",
		EncodingRGB24:          "rgb24",
		EncodingSyntheticScene: "synthetic_scene",
		Encoding(77):           "encoding(77)",
	}
	for enc, want := range cases {
		if got := enc.String(); got != want {
			t.Errorf("Encoding(%d) = %q, se esperaba %q", uint8(enc), got, want)
		}
	}
}

func TestPeekType(t *testing.T) {
	got, err := PeekType([]byte(`{"type":"client_hello","protocol_version":1}`))
	if err != nil || got != TypeClientHello {
		t.Errorf("PeekType = %q, %v", got, err)
	}
	if _, err := PeekType([]byte(`no es json`)); err == nil {
		t.Error("se esperaba error con JSON inválido")
	}
	if _, err := PeekType([]byte(`{"foo":1}`)); err == nil {
		t.Error("se esperaba error sin campo type")
	}
}

func TestMustParams(t *testing.T) {
	raw := MustParams(PoseParams{Action: "yaw_left"})
	if string(raw) != `{"action":"yaw_left"}` {
		t.Errorf("params = %s", raw)
	}
	// Un valor no serializable devuelve vacío en vez de romper la sesión.
	if got := MustParams(make(chan int)); got != nil {
		t.Errorf("params = %s, se esperaba nil", got)
	}
}
