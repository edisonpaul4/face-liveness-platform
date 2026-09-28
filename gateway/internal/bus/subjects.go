// Package bus conecta el gateway con los workers de análisis por NATS core.
//
// NATS core, nunca JetStream: el camino de frames es efímero. Un frame
// perdido se pierde; reintentarlo llega tarde y encima rompe la continuidad
// temporal de las señales.
//
// # Ruteo
//
//	analyzer.announce             worker → gateway   capacidad, periódico
//	analyzer.lease.<worker_id>    gateway → worker   asignación (request/reply)
//	session.<id>.frames           gateway → worker   frames, sin respuesta
//	session.<id>.features         worker → gateway   medidas por frame
//	session.<id>.heartbeat        worker → gateway   renovación del lease
//	session.<id>.control          gateway → worker   cierre y liberación
//
// # Afinidad de sesión
//
// Al arrancar, el gateway elige un worker y le asigna la sesión con un lease.
// A partir de ahí TODOS los frames de esa sesión van a ese worker, porque el
// estado caliente —tracking, ventana de rPPG, flujo óptico— vive en la
// memoria de ese worker. Ese estado NO viaja con cada frame: serializarlo
// costaría más que analizarlo y convertiría un flujo continuo en una sucesión
// de fotos sueltas.
//
// La otra cara de la moneda es que la sesión muere con el worker. Es una
// decisión, no un descuido: ver el lease.
package bus

import "strings"

// Subjects fijos.
const (
	// SubjectAnnounce es donde los workers anuncian su capacidad.
	SubjectAnnounce = "analyzer.announce"
	// SubjectLeasePrefix precede al identificador del worker.
	SubjectLeasePrefix = "analyzer.lease."
	// SubjectSessionPrefix precede al identificador de sesión.
	SubjectSessionPrefix = "session."
)

// Lease es el subject de asignación de un worker.
func Lease(workerID string) string { return SubjectLeasePrefix + workerID }

// Frames es el subject por el que viajan los frames de una sesión.
func Frames(sessionID string) string { return SubjectSessionPrefix + sessionID + ".frames" }

// Features es el subject por el que vuelven las medidas.
func Features(sessionID string) string { return SubjectSessionPrefix + sessionID + ".features" }

// Heartbeat es el subject por el que el worker renueva el lease.
func Heartbeat(sessionID string) string { return SubjectSessionPrefix + sessionID + ".heartbeat" }

// Control es el subject de higiene de la sesión.
func Control(sessionID string) string { return SubjectSessionPrefix + sessionID + ".control" }

// Window es por donde Go pide medir una ventana ya cerrada.
func Window(sessionID string) string { return SubjectSessionPrefix + sessionID + ".window" }

// ChallengeScore es por donde vuelve la medida de esa ventana.
func ChallengeScore(sessionID string) string {
	return SubjectSessionPrefix + sessionID + ".challenge_score"
}

// ValidToken indica si el identificador puede ir en un subject sin
// convertirse en comodín ni partirlo en dos.
//
// Los identificadores de sesión son ULID y los de worker los pone la
// configuración, pero un subject construido con datos sin validar es una vía
// para suscribirse a sesiones ajenas.
func ValidToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	return !strings.ContainsAny(s, ". *>\t\r\n")
}
