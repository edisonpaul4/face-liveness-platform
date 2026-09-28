"""Detectores de señal de PAD.

TODAVÍA SIN IMPLEMENTAR: son los pasos siguientes. Lo que sí funciona hoy es
la detección de rostro (`analyzer.detector`) y la calidad de captura
(`analyzer.quality`), que es el alcance de este paso.


Un detector = un módulo, con una función pura:

    compute(frame, state) -> dict[str, float]

Convención de nombres (CLAUDE.md §7): snake_case, familia como prefijo, sin
juicio de valor.

    OK        : texture_lbp_energy, moire_peak_ratio, rppg_snr_db
    PROHIBIDO : is_live, spoof_score, attack_detected

Cobertura prevista frente al modelo de amenaza (CLAUDE.md §5):

    texture   → A1 foto impresa
    moire     → A2 foto en pantalla, A3 video replay
    specular  → A1, A2
    depth     → A1, A2, A3
    flow      → A1, A3
    rppg      → A1, A2
    color     → A2, A3 (respuesta a iluminación activa)
"""
