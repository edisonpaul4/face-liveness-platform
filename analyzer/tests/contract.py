"""Validador del contrato de medidas.

Lo que sale del worker tiene que cumplir `/proto/nats/v1/frame_features.proto`.
Esta comprobación se aplica a TODOS los frames de los tests: un campo que se
cuela mal en un caso raro es un campo que se cuela mal en producción.
"""

from __future__ import annotations

from typing import Any

from analyzer import wire

#: Señales que tiene que emitir todo frame, haya rostro o no.
REQUIRED_SIGNALS = frozenset(
    {
        "face_count",
        "face_present",
        "quality_sharpness",
        "quality_brightness",
        "quality_highlight_saturation",
        "quality_contrast",
        "temporal_frames_seen",
        "temporal_window_frames",
    }
)


def assert_valid(features: dict[str, Any]) -> None:
    """Comprueba un mensaje de medidas contra el contrato.

    Raises:
        AssertionError: con el motivo exacto del incumplimiento.
    """
    for key in ("seq", "signals", "quality", "processing_ms", "version", "analyzed_at_us"):
        assert key in features, f"falta el campo obligatorio {key!r}"

    assert isinstance(features["seq"], int) and features["seq"] >= 0
    assert isinstance(features["processing_ms"], int) and features["processing_ms"] >= 0
    assert isinstance(features["version"], str) and features["version"]
    assert isinstance(features["analyzed_at_us"], int) and features["analyzed_at_us"] > 0

    signals = features["signals"]
    assert isinstance(signals, dict)
    missing = REQUIRED_SIGNALS - set(signals)
    assert not missing, f"faltan señales obligatorias: {sorted(missing)}"

    for name, value in signals.items():
        assert isinstance(value, (int, float)), f"la señal {name} no es numérica: {value!r}"
        assert name not in wire.FORBIDDEN_SIGNAL_NAMES, f"la señal {name} es un juicio"
        assert name == name.lower(), f"la señal {name} no está en snake_case"

    # Rangos: lo que dice ser una fracción tiene que serlo.
    assert 0.0 <= signals["quality_brightness"] <= 1.0
    assert 0.0 <= signals["quality_highlight_saturation"] <= 1.0
    assert 0.0 <= signals["quality_contrast"] <= 1.0
    assert signals["quality_sharpness"] >= 0.0
    assert signals["face_count"] >= 0
    assert signals["face_present"] in (0.0, 1.0)

    quality = features["quality"]
    for key in ("face_detected", "face_count", "sharpness", "brightness"):
        assert key in quality, f"falta quality.{key}"
    assert isinstance(quality["face_detected"], bool)
    assert quality["face_count"] == int(signals["face_count"])
    assert quality["face_detected"] == (signals["face_present"] == 1.0)

    # La caja está si y sólo si hay rostro.
    if quality["face_detected"]:
        assert "face" in features, "hay rostro pero no viene la caja"
        box = features["face"]
        for key in ("x", "y", "w", "h", "confidence"):
            assert key in box, f"falta face.{key}"
            assert isinstance(box[key], (int, float))
        assert 0.0 <= box["x"] <= 1.0 and 0.0 <= box["y"] <= 1.0, "la caja no está normalizada"
        assert 0.0 < box["w"] <= 1.0 and 0.0 < box["h"] <= 1.0, "la caja no tiene tamaño"
        assert box["x"] + box["w"] <= 1.001, "la caja se sale del frame por la derecha"
        assert box["y"] + box["h"] <= 1.001, "la caja se sale del frame por abajo"
        assert 0.0 <= box["confidence"] <= 1.0
    else:
        assert "face" not in features, "no hay rostro pero viene una caja"
