// Package evidence guarda en el almacén de objetos los frames y el clip de
// una sesión.
//
// La evidencia ES dato biométrico: una cara. Categoría especial bajo la LOPDP
// ecuatoriana (art. 25). De ahí las tres reglas de este paquete:
//
//  1. **Cifrado antes de salir del proceso.** AES-256-GCM en cliente, no
//     cifrado del lado del servidor: así el almacén nunca ve un píxel en
//     claro, ni siquiera si alguien se lleva el bucket entero.
//  2. **Caducidad corta por defecto.** Cada objeto nace con su fecha de
//     muerte.
//  3. **Borrado verificable.** Borrar y comprobar que ya no está; el recibo
//     lo guarda el registro de auditoría.
package evidence

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/oklog/ulid/v2"

	"github.com/edisonpaul4/biometrics/orchestrator/core/clock"
	"github.com/edisonpaul4/biometrics/orchestrator/internal/store/results"
)

// Kind es el tipo de evidencia.
type Kind string

// Tipos de evidencia.
const (
	// KindKeyFrame: frames sueltos del recorrido de la sesión.
	KindKeyFrame Kind = "key_frame"
	// KindReferenceFrame: el mejor frame. Es el que servirá para el match 1:1
	// posterior contra el documento, así que se elige por calidad y frontalidad,
	// no por orden.
	KindReferenceFrame Kind = "reference_frame"
	// KindClip: el vídeo completo.
	KindClip Kind = "clip"
)

// Algorithm identifica el cifrado usado.
const Algorithm = "aes-256-gcm"

// Errores del paquete.
var (
	ErrNotFound  = errors.New("evidence: objeto no encontrado")
	ErrBadKey    = errors.New("evidence: la clave de cifrado debe tener 32 bytes")
	ErrCorrupted = errors.New("evidence: el objeto no se pudo descifrar")
)

// Config configura el almacén.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool

	// Key es la clave AES-256. En producción sale de un KMS, no de un
	// fichero: aquí sólo se sostiene el material, no se gestiona su ciclo.
	Key []byte
	// KeyID identifica qué clave cifró cada objeto, para poder rotarlas sin
	// perder lo ya guardado.
	KeyID string
}

// Store guarda y recupera evidencia cifrada.
type Store struct {
	client *minio.Client
	bucket string
	aead   cipher.AEAD
	keyID  string
	clk    clock.Clock
}

// Open conecta con el almacén y prepara el cifrado.
func Open(ctx context.Context, cfg Config, clk clock.Clock) (*Store, error) {
	if len(cfg.Key) != 32 {
		return nil, fmt.Errorf("%w: llegaron %d", ErrBadKey, len(cfg.Key))
	}
	if clk == nil {
		clk = clock.System{}
	}

	block, err := aes.NewCipher(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("evidence: clave inválida: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("evidence: no se pudo preparar GCM: %w", err)
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("evidence: no se pudo conectar a %s: %w", cfg.Endpoint, err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("evidence: no se pudo consultar el bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("evidence: no se pudo crear el bucket: %w", err)
		}
	}

	return &Store{
		client: client, bucket: cfg.Bucket, aead: aead,
		keyID: cfg.KeyID, clk: clk,
	}, nil
}

// Put cifra y sube un objeto, y devuelve su apunte para el registro.
func (s *Store) Put(ctx context.Context, sessionID string, kind Kind, contentType string,
	payload []byte, expiresAt time.Time) (results.EvidenceObject, error) {

	if len(payload) == 0 {
		return results.EvidenceObject{}, errors.New("evidence: no se guarda un objeto vacío")
	}

	sealed, err := s.seal(payload)
	if err != nil {
		return results.EvidenceObject{}, err
	}

	objectID := ulid.Make().String()
	now := s.clk.Now().UTC()
	key := objectKey(now, sessionID, kind, objectID)

	// La huella es del CIFRADO. Prueba igual de bien que se borró este objeto
	// exacto, y no deja por ahí un identificador derivable de la persona.
	sum := sha256.Sum256(sealed)
	digest := hex.EncodeToString(sum[:])

	_, err = s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(sealed), int64(len(sealed)),
		minio.PutObjectOptions{
			ContentType: "application/octet-stream",
			UserMetadata: map[string]string{
				"liveness-session":    sessionID,
				"liveness-kind":       string(kind),
				"liveness-encryption": Algorithm,
				"liveness-key-id":     s.keyID,
				"liveness-expires-at": expiresAt.UTC().Format(time.RFC3339),
				// El tipo real del contenido va en metadatos, no en
				// Content-Type: el objeto que se sube son bytes cifrados.
				"liveness-content-type": contentType,
			},
		})
	if err != nil {
		return results.EvidenceObject{}, fmt.Errorf("evidence: no se pudo subir %s: %w", key, err)
	}

	return results.EvidenceObject{
		ObjectID:   objectID,
		SessionID:  sessionID,
		Kind:       string(kind),
		Bucket:     s.bucket,
		ObjectKey:  key,
		SizeBytes:  int64(len(sealed)),
		SHA256:     digest,
		Encryption: Algorithm,
		KeyID:      s.keyID,
		StoredAt:   now,
		ExpiresAt:  expiresAt.UTC(),
	}, nil
}

// Get descarga y descifra un objeto.
func (s *Store) Get(ctx context.Context, object results.EvidenceObject) ([]byte, error) {
	reader, err := s.client.GetObject(ctx, object.Bucket, object.ObjectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("evidence: no se pudo abrir %s: %w", object.ObjectKey, err)
	}
	defer reader.Close()

	sealed, err := io.ReadAll(reader)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("evidence: no se pudo leer %s: %w", object.ObjectKey, err)
	}
	return s.open(sealed)
}

// Delete borra un objeto del almacén.
func (s *Store) Delete(ctx context.Context, object results.EvidenceObject) error {
	err := s.client.RemoveObject(ctx, object.Bucket, object.ObjectKey, minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("evidence: no se pudo borrar %s: %w", object.ObjectKey, err)
	}
	return nil
}

// Exists comprueba si el objeto sigue estando.
//
// Es la mitad que convierte un borrado en un borrado verificable: borrar y no
// comprobar es confiar.
func (s *Store) Exists(ctx context.Context, object results.EvidenceObject) (bool, error) {
	_, err := s.client.StatObject(ctx, object.Bucket, object.ObjectKey, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("evidence: no se pudo comprobar %s: %w", object.ObjectKey, err)
}

// seal cifra con AES-256-GCM. El nonce va delante del texto cifrado.
func (s *Store) seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("evidence: no se pudo generar el nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// open descifra.
func (s *Store) open(sealed []byte) ([]byte, error) {
	size := s.aead.NonceSize()
	if len(sealed) < size {
		return nil, ErrCorrupted
	}
	plaintext, err := s.aead.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return plaintext, nil
}

// objectKey arma la ruta del objeto.
//
// Convención de CLAUDE.md §7: <yyyy>/<mm>/<dd>/<session_id>/<artefacto>. La
// fecha delante hace que borrar un día entero sea un prefijo.
func objectKey(now time.Time, sessionID string, kind Kind, objectID string) string {
	return fmt.Sprintf("%04d/%02d/%02d/%s/%s-%s",
		now.Year(), now.Month(), now.Day(), sessionID, kind, objectID)
}

func isNotFound(err error) bool {
	var response minio.ErrorResponse
	if errors.As(err, &response) {
		return response.Code == "NoSuchKey" || response.StatusCode == 404
	}
	return false
}
