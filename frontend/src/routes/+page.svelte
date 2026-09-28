<!--
	Pantalla de liveness.

	SCAFFOLDING: sin captura, sin conexión, sin lógica.

	Reglas para esta vista (CLAUDE.md §6):
	  · Muestra UN reto: el que el servidor acaba de emitir.
	  · No conoce cuántos retos hay ni cuáles vienen. No hay barra de progreso
	    "3 de 5" porque el cliente no sabe ni el 3 ni el 5.
	  · No decide si el reto se cumplió. Sólo envía frames.
	  · No contiene umbrales ni constantes de negocio.
-->
<script lang="ts">
	import ChallengePrompt from '$lib/components/ChallengePrompt.svelte';
	import CameraView from '$lib/components/CameraView.svelte';
</script>

<h1>Verificación de identidad</h1>
<p class="sub">Sitúa tu rostro dentro del marco y sigue las indicaciones.</p>

<CameraView />
<ChallengePrompt />

<p class="scaffold">scaffolding: sin lógica implementada</p>

<style>
	h1 {
		font-size: 20px;
		margin: 0 0 4px;
	}
	.sub {
		color: #8b93a1;
		font-size: 14px;
		margin: 0 0 16px;
	}
	.scaffold {
		margin-top: 24px;
		font-size: 12px;
		color: #6b7280;
	}
</style>
