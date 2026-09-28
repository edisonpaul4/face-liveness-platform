package bus_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edisonpaul4/biometrics/gateway/internal/bus"
)

// goldenDir son los vectores compartidos con el worker de Python.
//
// Los dos lados comprueban contra los mismos ficheros. Si alguien cambia una
// codificación y no la otra, esto salta antes de que un frame se pierda en
// producción por un byte movido.
const goldenDir = "../../../proto/testdata"

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("no se pudo leer el vector dorado %s: %v", name, err)
	}
	return data
}

// canonicalFrameTask es el frame del vector dorado.
func canonicalFrameTask() bus.FrameTask {
	return bus.FrameTask{
		Encoding:     1,
		Flags:        bus.FlagClientClockTrusted,
		Seq:          42,
		CapturedAtUS: 1_787_500_000_000_000,
		ReceivedAtUS: 1_787_500_000_045_000,
		Payload:      []byte("\xff\xd8\xff\xe0FRAME-PAYLOAD\xff\xd9"),
	}
}

func TestGoldenFrameTaskEncodesByteForByte(t *testing.T) {
	want := readGolden(t, "frame_task.bin")
	got := bus.EncodeFrameTask(canonicalFrameTask())

	if len(got) != len(want) {
		t.Fatalf("el frame ocupa %d bytes y el vector dorado %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d: 0x%02x, el vector dorado dice 0x%02x", i, got[i], want[i])
		}
	}
}

func TestGoldenFrameTaskDecodesToExpectedValues(t *testing.T) {
	var expected struct {
		Version            uint8  `json:"version"`
		Encoding           uint8  `json:"encoding"`
		Flags              uint16 `json:"flags"`
		Seq                uint64 `json:"seq"`
		CapturedAtUS       int64  `json:"captured_at_us"`
		ReceivedAtUS       int64  `json:"received_at_us"`
		ClientClockTrusted bool   `json:"client_clock_trusted"`
		PayloadLen         int    `json:"payload_len"`
		PayloadBase64      string `json:"payload_base64"`
	}
	if err := json.Unmarshal(readGolden(t, "frame_task.expected.json"), &expected); err != nil {
		t.Fatalf("valores esperados ilegibles: %v", err)
	}

	got, err := bus.DecodeFrameTask(readGolden(t, "frame_task.bin"))
	if err != nil {
		t.Fatalf("DecodeFrameTask: %v", err)
	}

	if got.Encoding != expected.Encoding || got.Flags != expected.Flags || got.Seq != expected.Seq {
		t.Errorf("cabecera %+v, se esperaba %+v", got, expected)
	}
	if got.CapturedAtUS != expected.CapturedAtUS || got.ReceivedAtUS != expected.ReceivedAtUS {
		t.Errorf("sellos de tiempo: capturado=%d recibido=%d", got.CapturedAtUS, got.ReceivedAtUS)
	}
	if got.ClientClockTrusted() != expected.ClientClockTrusted {
		t.Errorf("ClientClockTrusted = %v", got.ClientClockTrusted())
	}
	if base64.StdEncoding.EncodeToString(got.Payload) != expected.PayloadBase64 {
		t.Errorf("el payload no coincide con el vector dorado")
	}
}

// TestGoldenFeaturesParse: lo que Python emite, Go lo entiende.
func TestGoldenFeaturesParse(t *testing.T) {
	var features bus.FrameFeatures
	if err := json.Unmarshal(readGolden(t, "frame_features.json"), &features); err != nil {
		t.Fatalf("medidas ilegibles: %v", err)
	}

	if features.Seq != 42 {
		t.Errorf("seq = %d", features.Seq)
	}
	if !features.Quality.FaceDetected || features.Quality.FaceCount != 1 {
		t.Errorf("calidad = %+v", features.Quality)
	}
	for _, name := range []string{"quality_sharpness", "quality_brightness", "quality_highlight_saturation"} {
		if _, ok := features.Signals[name]; !ok {
			t.Errorf("falta la señal %s", name)
		}
	}
	if features.Version == "" {
		t.Error("las medidas no dicen con qué versión se calcularon")
	}
}

// TestGoldenControlMessages: los mensajes de control del contrato.
func TestGoldenControlMessages(t *testing.T) {
	var announce bus.Announce
	if err := json.Unmarshal(readGolden(t, "announce.json"), &announce); err != nil {
		t.Fatalf("anuncio ilegible: %v", err)
	}
	if announce.WorkerID == "" || announce.Capacity == 0 {
		t.Errorf("anuncio incompleto: %+v", announce)
	}

	var lease bus.LeaseRequest
	if err := json.Unmarshal(readGolden(t, "lease_request.json"), &lease); err != nil {
		t.Fatalf("lease ilegible: %v", err)
	}
	if lease.SessionID == "" || lease.DeadlineUS == 0 {
		t.Errorf("lease incompleto: %+v", lease)
	}

	var beat bus.SessionHeartbeat
	if err := json.Unmarshal(readGolden(t, "session_heartbeat.json"), &beat); err != nil {
		t.Fatalf("heartbeat ilegible: %v", err)
	}
	if beat.WorkerID == "" || beat.SessionID == "" {
		t.Errorf("heartbeat incompleto: %+v", beat)
	}

	var control bus.SessionControl
	if err := json.Unmarshal(readGolden(t, "session_control.json"), &control); err != nil {
		t.Fatalf("control ilegible: %v", err)
	}
	if control.Kind != bus.ControlSessionClose {
		t.Errorf("kind = %q, se esperaba %q", control.Kind, bus.ControlSessionClose)
	}
}
