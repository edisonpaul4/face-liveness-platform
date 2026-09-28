package fusion

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Este fichero es la ÚNICA E/S de core/fusion. Está aquí, y no en un paquete
// interno, porque la recarga en caliente es parte del requisito: mover un
// umbral no puede costar un despliegue. El resto del paquete es puro y se
// prueba sin tocar el disco.

// LoadFile lee y valida un perfil desde disco.
func LoadFile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fusion: no se pudo leer el perfil %s: %w", path, err)
	}
	profile, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("fusion: perfil %s: %w", path, err)
	}
	return profile, nil
}

// Loader vigila el perfil en disco y lo recarga cuando cambia.
type Loader struct {
	path   string
	engine *Engine
	log    *slog.Logger

	lastModified time.Time
	lastSize     int64
}

// NewLoader crea el vigilante. No lee nada todavía.
func NewLoader(path string, engine *Engine, log *slog.Logger) *Loader {
	if log == nil {
		log = slog.Default()
	}
	return &Loader{path: path, engine: engine, log: log}
}

// Reload relee el perfil si el fichero cambió. Devuelve si hubo cambio.
//
// Un perfil inválido NO sustituye al que está en vigor: se registra el error y
// se sigue decidiendo con el anterior. Un fichero a medio escribir no puede
// dejar el sistema sin criterio.
func (l *Loader) Reload() (bool, error) {
	info, err := os.Stat(l.path)
	if err != nil {
		return false, fmt.Errorf("fusion: no se pudo consultar %s: %w", l.path, err)
	}

	if info.ModTime().Equal(l.lastModified) && info.Size() == l.lastSize {
		return false, nil
	}

	profile, err := LoadFile(l.path)
	if err != nil {
		// Ojo: no se actualiza lastModified, así que se reintentará en el
		// siguiente ciclo. Un fichero a medio escribir se recupera solo.
		return false, err
	}
	if err := l.engine.Reload(profile); err != nil {
		return false, err
	}

	l.lastModified = info.ModTime()
	l.lastSize = info.Size()
	l.log.Info("perfil de decisión recargado",
		"path", l.path, "version", profile.Version, "checksum", profile.Checksum())
	return true, nil
}

// Watch recarga periódicamente hasta que se cancele el contexto.
func (l *Loader) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := l.Reload(); err != nil {
				l.log.Error("no se pudo recargar el perfil; sigue el anterior",
					"path", l.path, "err", err,
					"version_en_vigor", l.engine.Profile().Version)
			}
		}
	}
}
