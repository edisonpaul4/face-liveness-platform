# Makefile de la plataforma de liveness.
#
# Targets principales: up, down, test, lint
# Los targets se saltan con aviso lo que no esté instalado (Go, Python, Node),
# para que un clon limpio siempre pueda ejecutarlos.

SHELL := /bin/bash
.DEFAULT_GOAL := help
.SILENT:

# Carga .env si existe, para que los mensajes de este Makefile usen los
# mismos puertos que docker compose.
-include .env

NATS_PORT ?= 4222
NATS_MONITOR_PORT ?= 8222
REDIS_PORT ?= 6379
POSTGRES_PORT ?= 5432
MINIO_PORT ?= 9000
MINIO_CONSOLE_PORT ?= 9001

COMPOSE := docker compose

# Intérprete del analyzer: su entorno virtual si existe.
ANALYZER_PY := $(CURDIR)/analyzer/.venv/bin/python
GO_MODULES := gateway orchestrator

# A qué gateway apuntan el cliente de pruebas y su banco.
GATEWAY_URL ?= http://localhost:8080
# Origen desde el que se sirve el cliente. El gateway sólo atiende a los
# orígenes que se le nombren: el navegador no llega si no está en la lista.
WEB_ORIGIN  ?= http://localhost:$(WEB_PORT)
WEB_PORT    ?= 5173

.PHONY: help up down restart ps logs test lint fmt deps venv models analyzer gateway web bench-web replay flash-colors analyze datasets bench-dataset web-fixtures clean nuke test-go test-py test-web test-client lint-go lint-py lint-web

help: ## Muestra esta ayuda
	grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- infra ----

up: ## Levanta la infraestructura (NATS, Redis, Postgres, MinIO)
	$(COMPOSE) up -d --wait nats redis postgres minio
	$(COMPOSE) up -d minio-init
	echo ""
	echo "  NATS      nats://localhost:$(NATS_PORT)   (monitor :$(NATS_MONITOR_PORT))"
	echo "  Redis     redis://localhost:$(REDIS_PORT)"
	echo "  Postgres  postgres://localhost:$(POSTGRES_PORT)/liveness"
	echo "  MinIO     http://localhost:$(MINIO_PORT)   (consola :$(MINIO_CONSOLE_PORT))"
	echo ""

down: ## Baja la infraestructura (conserva volúmenes)
	$(COMPOSE) down --remove-orphans

restart: down up ## Reinicia la infraestructura

ps: ## Estado de los contenedores
	$(COMPOSE) ps

logs: ## Sigue los logs de la infraestructura
	$(COMPOSE) logs -f --tail=100

## ---------------------------------------------------------------- tests ----

test: test-go test-py test-web ## Ejecuta los tests de todos los componentes
	echo "==> tests completados"

test-go:
	if ! command -v go >/dev/null 2>&1; then \
		echo "==> [skip] Go no instalado"; exit 0; \
	fi; \
	for m in $(GO_MODULES); do \
		echo "==> go test ./$$m"; \
		( cd $$m && go test ./... ) || exit 1; \
	done

test-py:
	PY=$(ANALYZER_PY); \
	if [ ! -x "$$PY" ]; then \
		if command -v python3 >/dev/null 2>&1 && python3 -c "import pytest" >/dev/null 2>&1; then \
			PY=python3; \
		else \
			echo "==> [skip] analyzer sin entorno (ejecuta: make venv)"; exit 0; \
		fi; \
	fi; \
	echo "==> pytest analyzer"; \
	( cd analyzer && "$$PY" -m pytest ); \
	code=$$?; \
	if [ $$code -eq 5 ]; then echo "==> analyzer: sin tests todavia (ok)"; exit 0; fi; \
	exit $$code

test-web: test-client
	if [ ! -d frontend/node_modules ]; then \
		echo "==> [skip] frontend sin dependencias (cd frontend && npm install)"; exit 0; \
	fi; \
	echo "==> npm test frontend"; \
	( cd frontend && npm test --if-present )

test-client:
	echo "==> node --test web/tests"
	node --test web/tests/*.test.mjs

## ----------------------------------------------------------------- lint ----

lint: lint-go lint-py lint-web ## Ejecuta los linters de todos los componentes
	echo "==> lint completado"

lint-go:
	if ! command -v go >/dev/null 2>&1; then \
		echo "==> [skip] Go no instalado"; exit 0; \
	fi; \
	for m in $(GO_MODULES); do \
		echo "==> go vet ./$$m"; \
		( cd $$m && gofmt -l . && go vet ./... ) || exit 1; \
	done; \
	if command -v golangci-lint >/dev/null 2>&1; then \
		for m in $(GO_MODULES); do ( cd $$m && golangci-lint run ) || exit 1; done; \
	else \
		echo "==> [skip] golangci-lint no instalado"; \
	fi

lint-py:
	PY=$(ANALYZER_PY); \
	if [ -x "$$PY" ] && "$$PY" -m ruff --version >/dev/null 2>&1; then \
		echo "==> ruff analyzer"; ( cd analyzer && "$$PY" -m ruff check . ) || exit 1; \
	elif command -v ruff >/dev/null 2>&1; then \
		echo "==> ruff analyzer"; ruff check analyzer || exit 1; \
	else \
		echo "==> [skip] ruff no instalado"; \
	fi; \
	if [ -x "$$PY" ] && "$$PY" -m mypy --version >/dev/null 2>&1; then \
		echo "==> mypy analyzer"; ( cd analyzer && "$$PY" -m mypy ) || exit 1; \
	elif command -v mypy >/dev/null 2>&1; then \
		echo "==> mypy analyzer"; ( cd analyzer && mypy ) || exit 1; \
	else \
		echo "==> [skip] mypy no instalado"; \
	fi

lint-web:
	if [ ! -d frontend/node_modules ]; then \
		echo "==> [skip] frontend sin dependencias"; exit 0; \
	fi; \
	( cd frontend && npm run lint --if-present )

## ------------------------------------------------------------ utilidades ----

fmt: ## Formatea el código
	if command -v go >/dev/null 2>&1; then for m in $(GO_MODULES); do ( cd $$m && gofmt -w . ); done; fi
	if command -v ruff >/dev/null 2>&1; then ruff format analyzer; fi
	if [ -d frontend/node_modules ]; then ( cd frontend && npm run format --if-present ); fi

venv: ## Crea el entorno del analyzer e instala sus dependencias
	python3 -m venv analyzer/.venv
	$(ANALYZER_PY) -m pip install --quiet --upgrade pip
	$(ANALYZER_PY) -m pip install --quiet -e "analyzer[dev]"
	@echo "==> entorno listo en analyzer/.venv"

models: ## Descarga los modelos de detección (no se versionan)
	@if [ -x "$(ANALYZER_PY)" ]; then \
		$(ANALYZER_PY) $(CURDIR)/analyzer/scripts/fetch_models.py; \
	else \
		python3 analyzer/scripts/fetch_models.py; \
	fi

# CAPTURE elige la resolución impuesta al cliente: 480 (por defecto) o 720.
#
# 480p es el valor por defecto porque a 720p el cliente pedía **20,7 Mbit/s**
# de subida —206 KB por frame— y desde un móvil eso va al límite del enlace:
# medido, un parón de 1,8 s se llevó por delante un reto de mirada entero,
# justo el tramo donde el punto saltaba.
#
# Y no se paga con precisión, que era lo que había que comprobar antes de
# bajar. Medido sobre 24 frames reales de una sesión con la cara cerca:
#
#     720p q92   158 KB   4 de 24 frames sin poder medir el iris
#     480p q92    58 KB   1 de 24        error de iris 0,0054
#     480p q80    33 KB   0 de 24        error de iris 0,0073
#
# El error es el 20 % de lo que mueve una mirada real (0,027), y el detector
# pierde MENOS frames: con la cara ocupando medio encuadre, a 720p le sobran
# píxeles. El antecedente apunta al mismo sitio — el destello dejó de
# correlacionar precisamente al subir a 720p, seis sesiones de seis.
#
# El interruptor se queda para poder volver a comparar en la misma habitación:
# sin él, la resolución no se puede separar del resto de variables de la escena.
CAP_W = $(if $(filter 720,$(CAPTURE)),1280,640)
CAP_H = $(if $(filter 720,$(CAPTURE)),720,480)
# 30 fps a 480p, 15 a 720p: es aproximadamente el mismo caudal de subida por
# los dos caminos (14,4 y 10,4 Mbit/s), y así el interruptor cambia la
# resolución sin cambiar a la vez la carga de red — que es lo único que hace
# comparables las dos medidas.
CAP_FPS = $(if $(filter 720,$(CAPTURE)),15,30)

gateway: ## Arranca el gateway (CAPTURE=720 vuelve a 720p · SKIP=flash,gaze excluye retos · EXPLAIN=1 modo pentest)
	@echo "==> captura impuesta: $(CAP_W)x$(CAP_H) @ $(CAP_FPS) fps"
	cd gateway && GATEWAY_ALLOWED_ORIGINS=$(WEB_ORIGIN) \
		GATEWAY_CAPTURE_WIDTH=$(CAP_W) \
		GATEWAY_CAPTURE_HEIGHT=$(CAP_H) \
		GATEWAY_MAX_FPS=$(CAP_FPS) \
		GATEWAY_INSECURE_SKIP_CHALLENGES=$(SKIP) \
		GATEWAY_INSECURE_EXPLAIN_VERDICT=$(if $(EXPLAIN),true,false) go run ./cmd/gateway

web: ## Sirve el cliente de pruebas (WEB_PORT, 5173 por defecto)
	node web/serve.mjs $(WEB_PORT) --gateway $(GATEWAY_URL)

bench-web: ## Juega una sesión completa con el código del cliente, sin navegador
	@if [ ! -d web/harness/fixtures ]; then $(MAKE) web-fixtures; fi
	node web/harness/session-harness.mjs $(GATEWAY_URL)
	@echo "recuerda: el maniquí no sabe hacer pitch_up ni gaze."
	@echo "para ejercitarlo entero: make gateway SKIP=gaze  (y sin poses de pitch)"

replay: ## Reproduce una grabación contra el analizador (REC=<fichero>)
	@if [ -z "$(REC)" ]; then \
		echo "uso: make replay REC=web/harness/recordings/session-....bin"; \
		ls -1 web/harness/recordings/*.bin 2>/dev/null | tail -5; exit 1; \
	fi
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/bench/runner/replay.py $(REC)

datasets: ## Descarga las muestras públicas de datasets de ataques
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/bench/runner/fetch_datasets.py $(if $(LIST),--list,)

bench-dataset: ## Matriz APCER/BPCER sobre un dataset (DS=<directorio>)
	@if [ -z "$(DS)" ]; then echo "uso: make bench-dataset DS=~/datasets/liveness/axon-fas"; exit 1; fi
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/bench/runner/bench_dataset.py $(DS) $(if $(LIMIT),--limit $(LIMIT),)

analyze: ## Analiza una foto o vídeo con los detectores pasivos (MEDIA=<fichero>)
	@if [ -z "$(MEDIA)" ]; then echo "uso: make analyze MEDIA=ataque.mp4"; exit 1; fi
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/bench/runner/analyze_media.py $(MEDIA)

flash-colors: ## Mide la respuesta de cada color del destello (REC=<fichero>)
	@if [ -z "$(REC)" ]; then \
		echo "uso: make flash-colors REC=web/harness/recordings/session-....bin"; \
		ls -1 web/harness/recordings/*.bin 2>/dev/null | tail -5; exit 1; \
	fi
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/bench/runner/flash_colors.py $(REC)

web-fixtures: ## Regenera los frames sintéticos que usa bench-web
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	$(ANALYZER_PY) $(CURDIR)/web/harness/make-fixtures.py

analyzer: ## Arranca el worker de análisis contra el NATS local
	@if [ ! -x "$(ANALYZER_PY)" ]; then echo "falta el entorno: make venv"; exit 1; fi
	cd analyzer && .venv/bin/python -m analyzer

deps: ## Descarga dependencias (requiere red la primera vez)
	if command -v go >/dev/null 2>&1; then \
		for m in $(GO_MODULES); do echo "==> go mod download ./$$m"; ( cd $$m && go mod download ); done; \
	else echo "==> [skip] Go no instalado"; fi
	echo "==> analyzer: make venv && make models"
	echo "==> frontend: cd frontend && npm install       (manual)"

clean: ## Limpia artefactos de build locales
	rm -rf gateway/bin orchestrator/bin frontend/.svelte-kit frontend/build
	find . -name '__pycache__' -type d -prune -exec rm -rf {} + 2>/dev/null || true
	find . -name '.pytest_cache' -type d -prune -exec rm -rf {} + 2>/dev/null || true

nuke: ## Baja la infraestructura y BORRA los volúmenes (datos incluidos)
	$(COMPOSE) down -v --remove-orphans
