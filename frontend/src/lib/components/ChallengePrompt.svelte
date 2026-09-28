<!--
	Muestra el reto activo.

	PROHIBIDO en este componente (CLAUDE.md §6):
	  · Barra o texto de progreso tipo "reto 3 de 5".
	  · Precargar, anticipar o animar el reto siguiente.
	  · Mostrar el resultado de un reto individual.

	El texto se resuelve desde `prompt_key` con el catálogo i18n local: el
	servidor manda claves, no copy.

	SCAFFOLDING: sin implementación.
-->
<script lang="ts">
	let text = $state<string>('—');
	// TODO(scaffolding): suscribirse al reto activo y traducir prompt_key.
</script>

<div class="prompt">
	<p>{text}</p>
</div>

<style>
	.prompt {
		margin-top: 16px;
		min-height: 56px;
		display: grid;
		place-items: center;
		border: 1px solid #262a33;
		border-radius: 8px;
		font-size: 17px;
	}
</style>
