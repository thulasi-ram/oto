// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightThemeBlack from 'starlight-theme-black';

// https://astro.build/config
export default defineConfig({
	integrations: [
		starlight({
			title: 'oto',
			description: 'The alert history layer your Prometheus stack does not have.',
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/thulasi-ram/oto' }],
			plugins: [starlightThemeBlack({})],
			customCss: ['./src/styles/custom.css'],
			sidebar: [
				{
					label: 'Overview',
					items: [
						{ label: 'Introduction', slug: 'guides/overview' },
						{ label: 'Architecture', slug: 'architecture' },
						{ label: 'Orchestration', slug: 'orchestration' },
					],
				},
				{ label: 'Design', items: [{ autogenerate: { directory: 'design' } }] },
				{ label: 'ADRs', items: [{ autogenerate: { directory: 'adr' } }] },
				{ label: 'Setup', items: [{ autogenerate: { directory: 'setup' } }] },
				{ label: 'Runbooks', items: [{ autogenerate: { directory: 'runbooks' } }] },
			],
		}),
	],
});
