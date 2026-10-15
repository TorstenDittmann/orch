import { mount } from 'svelte';
import App from './App.svelte';
import './app.css';
import { applyTheme, theme } from './lib/state.svelte';

applyTheme(theme.value);

export default mount(App, { target: document.getElementById('app')! });
