<script lang="ts">
	let { children } = $props();
</script>

<main>
	{@render children()}
</main>

<style>
	:global(body) {
		margin: 0;
		font-family: system-ui, sans-serif;
		background: #0f1115;
		color: #e6e6e6;
	}
	main {
		max-width: 640px;
		margin: 0 auto;
		padding: 24px 16px;
	}
</style>
