"""Las dos señales que miran al SENSOR, no al sujeto.

Todo lo demás del sistema pregunta si hay una cara viva delante de la cámara.
Estas dos preguntan otra cosa: si hay una cámara. Distinguen un sensor físico
de un flujo de píxeles inyectado, que es una clase de ataque —A5— que puentea
la superficie de presentación entera: sin papel, sin pantalla, sin moiré, sin
bisel, y con los retos activos superados por una persona real cuya cara se
sustituye en vuelo.

Lo que NO prueban, y conviene tenerlo escrito: un replay en pantalla delante
de una cámara real produce obturador rodante y respuesta 3A genuinos, porque
hay un sensor de verdad barriendo. Son un eje distinto, no uno mejor.
"""

from __future__ import annotations

import numpy as np

from analyzer import flash
from analyzer.calibration import Baseline
from analyzer.photometry import ROW_BANDS, FramePhotometry

SEQUENCE = (
    flash.FlashSegment(color="red", duration_ms=500),
    flash.FlashSegment(color="green", duration_ms=500),
)
SPEC = flash.FlashWindowSpec(
    window_id="w", sequence=SEQUENCE, started_at_us=0, ended_at_us=1_000_000
)
FPS_US = 33_000


def frame(rows_bgr: np.ndarray, background: np.ndarray) -> FramePhotometry:
    return FramePhotometry(
        regions=np.tile(rows_bgr.mean(axis=0), (4, 1)),
        background=background,
        face_luminance=float(rows_bgr.mean()),
        specular_ratio=0.0,
        luminance_ratio=1.0,
        moire_index=0.0,
        banding_index=0.0,
        row_profile=rows_bgr,
    )


def scene(*, rolling: bool, agc: bool) -> list[tuple[int, FramePhotometry]]:
    """Ventana de destello sintética.

    `rolling`: el cambio de color cae a mitad de barrido del sensor.
    `agc`: el control automático de la cámara reacciona, y se ve en el FONDO.
    """
    out = []
    switch_us = 500_000  # el rojo pasa a verde aquí
    for i in range(30):
        at = i * FPS_US
        rojo = np.array([0.0, 0.0, 200.0])
        verde = np.array([0.0, 200.0, 0.0])

        # Gradiente vertical propio del rostro: la frente recibe más luz que
        # la barbilla. Toda cara real lo tiene, y si no se cancela produce una
        # falsa señal de obturador rodante — pasó en la primera sesión real.
        sombra = np.linspace(1.25, 0.75, ROW_BANDS)[:, None]

        if not rolling or abs(at - switch_us) > FPS_US / 2:
            colour = rojo if at < switch_us else verde
            rows = np.tile(colour, (ROW_BANDS, 1)) * sombra
        else:
            # Frame de transición: las bandas de arriba con el color viejo, las
            # de abajo con el nuevo. Es lo que hace un obturador rodante.
            corte = ROW_BANDS // 2
            rows = np.vstack([
                np.tile(rojo, (corte, 1)),
                np.tile(verde, (ROW_BANDS - corte, 1)),
            ]) * sombra

        # El fondo NO recibe el destello. Si se mueve es la cámara corrigiendo.
        #
        # Y reacciona a la LUMINANCIA, no a la media de canales: el verde
        # aporta el doble de luz que el rojo. Con la media plana los tres
        # primarios saturados valen lo mismo y no habría nada que medir — que
        # es justo el fallo que este test destapó en `sensor_response`.
        fondo = np.array([60.0, 60.0, 60.0])
        if agc:
            luz = float(np.dot(rows.mean(axis=0), [0.114, 0.587, 0.299]))
            fondo = fondo * (1.0 - 0.003 * luz)
        out.append((at, frame(rows, fondo)))
    return out


def test_el_obturador_rodante_se_ve_en_un_sensor_y_no_en_un_flujo_compuesto() -> None:
    real = flash.row_gradient(SPEC, scene(rolling=True, agc=True), 0)
    inyectado = flash.row_gradient(SPEC, scene(rolling=False, agc=False), 0)

    assert real is not None and inyectado is not None
    assert real > 0.4, f"un sensor real debería partir el frame de transición, dio {real:.3f}"
    assert inyectado < 0.05, (
        f"un flujo compuesto cambia el frame entero de golpe y no tiene "
        f"frontera; dio {inyectado:.3f}"
    )


def test_el_control_automatico_mueve_el_fondo_y_un_flujo_inyectado_no() -> None:
    """El fondo no recibe el destello: si se mueve, es la cámara."""
    real = flash.sensor_response(SPEC, scene(rolling=True, agc=True), 0)
    inyectado = flash.sensor_response(SPEC, scene(rolling=True, agc=False), 0)

    assert real is not None
    assert real > 0.8, f"la cámara debería seguir al estímulo, dio {real:.3f}"
    assert inyectado is None or inyectado < 0.3, (
        f"sin sensor no hay lazo de control que reaccione; dio {inyectado}"
    )


def test_sin_perfil_de_filas_no_se_inventa_una_medida() -> None:
    """Rostro pequeño o frames sin landmarks: se dice que no, no se rellena."""
    muestras = [(at, p) for at, p in scene(rolling=True, agc=True)]
    sin_filas = [
        (
            at,
            FramePhotometry(
                regions=p.regions,
                background=p.background,
                face_luminance=p.face_luminance,
                specular_ratio=0.0,
                luminance_ratio=1.0,
                moire_index=0.0,
                banding_index=0.0,
                row_profile=None,
            ),
        )
        for at, p in muestras
    ]
    assert flash.row_gradient(SPEC, sin_filas, 0) is None


def test_ninguna_de_las_dos_entra_en_el_score() -> None:
    """Van en `raw`, no en las sub-métricas.

    Están sin calibrar contra ataques reales. Darles voto en el resumen 0-1
    antes de medirlas sería el mismo error que este repositorio ya evitó con
    los clasificadores pasivos.
    """
    from analyzer.calibration import Baseline

    base = Baseline(
        region_ratio=np.full((4, 3), 0.8),
        region_noise=np.full((4, 3), 0.02),
        face_luminance=110.0,
        specular_ratio=0.0,
        luminance_ratio=0.7,
        moire_index=0.0,
        banding_index=0.0,
        frames=24,
    )
    score = flash.analyze(SPEC, scene(rolling=True, agc=True), base, None)

    assert "sensor_rolling_shutter" in score.raw
    assert "sensor_agc_response" in score.raw
    assert set(score.submetrics) == {"correlation", "gradient_3d", "screen_absence"}


def test_una_cara_muy_sombreada_no_finge_obturador_rodante() -> None:
    """El fallo que se coló hasta la primera sesión real.

    La primera versión medía la diferencia entre la banda de abajo y la de
    arriba del frame de transición, en absoluto. Pero toda cara tiene sombra
    vertical propia, esa sombra se proyecta sobre la dirección del cambio de
    color, y la señal salía clavada en 1,000 —el tope de la escala— sin que
    hubiera barrido ninguno.

    Aquí se exagera la sombra a propósito: si la medida es correcta, un flujo
    compuesto sigue dando cerca de cero por muy sombreada que esté la cara.
    """
    import analyzer.photometry as photometry

    original = photometry.ROW_BANDS
    fuerte = []
    switch_us = 500_000
    for i in range(30):
        at = i * FPS_US
        rojo = np.array([0.0, 0.0, 200.0])
        verde = np.array([0.0, 200.0, 0.0])
        # Sombra brutal: la barbilla recibe un tercio de la luz de la frente.
        sombra = np.linspace(1.5, 0.5, original)[:, None]
        colour = rojo if at < switch_us else verde
        rows = np.tile(colour, (original, 1)) * sombra
        fuerte.append((at, frame(rows, np.array([60.0, 60.0, 60.0]))))

    valor = flash.row_gradient(SPEC, fuerte, 0)
    assert valor is not None
    assert valor < 0.05, (
        f"la sombra propia del rostro se está leyendo como barrido: {valor:.3f}"
    )


# --- dispersión subsuperficial -------------------------------------------

SEQ_AZUL = (
    flash.FlashSegment(color="blue", duration_ms=500),
    flash.FlashSegment(color="green", duration_ms=500),
)
SPEC_AZUL = flash.FlashWindowSpec(
    window_id="w", sequence=SEQ_AZUL, started_at_us=0, ended_at_us=1_000_000
)


def base_neutra() -> Baseline:
    return Baseline(
        region_ratio=np.full((4, 3), 0.8),
        region_noise=np.full((4, 3), 0.02),
        face_luminance=110.0,
        specular_ratio=0.0,
        luminance_ratio=0.7,
        moire_index=0.0,
        banding_index=0.0,
        frames=24,
    )


def piel(*, subsuperficial: float) -> list[tuple[int, FramePhotometry]]:
    """Ventana de destello sobre una superficie con —o sin— translucidez.

    `subsuperficial` es cuánto rojo devuelve de más, por encima de lo que el
    color emitido explica. Una cara viva lo hace; el papel y la silicona no.
    """
    out = []
    fondo = np.array([100.0, 100.0, 100.0])
    for i in range(30):
        at = i * FPS_US
        colour = flash.stimulus_at(SPEC_AZUL, at, 0)
        if colour is None:
            colour = np.zeros(3)
        c = np.asarray(colour, dtype=np.float64)
        n = np.linalg.norm(c)
        directo = (c / n) if n > 0 else np.zeros(3)

        # Reposo 0,8 de la respuesta del fondo, más la respuesta al destello.
        ratio = np.full(3, 0.8) * np.exp(0.30 * directo)
        # Y la translucidez: rojo que sale sin que nadie lo haya mandado.
        ratio = ratio * np.exp(np.array([0.0, 0.0, subsuperficial]))

        regiones = np.tile(ratio * fondo, (4, 1))
        out.append((at, FramePhotometry(
            regions=regiones, background=fondo, face_luminance=110.0,
            specular_ratio=0.0, luminance_ratio=1.0, moire_index=0.0,
            banding_index=0.0,
        )))
    return out


def test_la_piel_viva_devuelve_rojo_que_el_destello_no_le_mandó() -> None:
    viva = flash.subsurface_red(SPEC_AZUL, piel(subsuperficial=0.12), base_neutra(), 0)
    plana = flash.subsurface_red(SPEC_AZUL, piel(subsuperficial=0.0), base_neutra(), 0)

    assert viva is not None and plana is not None
    assert viva > 0.15, f"la piel translúcida debería devolver rojo de más, dio {viva:.3f}"
    assert plana < 0.05, f"una superficie opaca no debería devolverlo, dio {plana:.3f}"


def test_una_secuencia_toda_roja_no_es_medible() -> None:
    """Con un destello rojo, el rojo devuelto viene del propio estímulo.

    No dice nada sobre translucidez, así que se declara no medible en vez de
    devolver un número que parecería una medida.
    """
    solo_rojo = flash.FlashWindowSpec(
        window_id="w",
        sequence=(
            flash.FlashSegment(color="red", duration_ms=500),
            flash.FlashSegment(color="red", duration_ms=500),
        ),
        started_at_us=0,
        ended_at_us=1_000_000,
    )
    assert flash.subsurface_red(solo_rojo, piel(subsuperficial=0.12), base_neutra(), 0) is None
