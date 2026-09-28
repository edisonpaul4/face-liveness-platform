// Package bus conecta el orchestrator al bus NATS core.
//
//   - Suscripción a session.<id>.features (medidas del analyzer).
//   - Suscripción a analyzer.announce (capacidad y versiones de los workers).
//   - Publicación en session.<id>.control, exclusivamente para higiene de
//     recursos (session_close). Nunca semántica de negocio.
//
// El ruteo y la asignación por lease están descritos en /proto/subjects.md.
//
// SCAFFOLDING: sin implementación.
package bus
