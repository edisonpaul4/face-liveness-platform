<!--
	Vista de cámara y superficie de iluminación activa.

	La capa de iluminación pinta la pantalla del color que el servidor imponga
	durante un reto `flash`. Es la contramedida principal contra
	el ataque A3 (video replay): un vídeo grabado no puede responder a un color
	elegido ahora.

	El color LO DECIDE EL SERVIDOR y llega en el reto activo, como secuencia de
	{color, duration_ms}. El cliente nunca lo elige ni lo predice.

	SCAFFOLDING: sin implementación.
-->
<script lang="ts">
	let video = $state<HTMLVideoElement | undefined>(undefined);
	// Impuesto por el servidor; `transparent` mientras no haya reto de iluminación.
	let illuminationColor = $state<string>('transparent');
	// TODO(scaffolding): getUserMedia, ciclo de captura, envío de frames.
</script>

<div class="frame">
	<!-- svelte-ignore a11y_media_has_caption -->
	<video bind:this={video} autoplay playsinline muted></video>
	<div class="illumination" style:background={illuminationColor}></div>
</div>

<style>
	.frame {
		position: relative;
		aspect-ratio: 3 / 4;
		background: #000;
		border-radius: 12px;
		overflow: hidden;
	}
	video {
		width: 100%;
		height: 100%;
		object-fit: cover;
	}
	.illumination {
		position: absolute;
		inset: 0;
		pointer-events: none;
		transition: background 60ms linear;
	}
</style>
