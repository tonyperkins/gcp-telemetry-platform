import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';

// Root CSS Imports
import 'leaflet/dist/leaflet.css';
import './styles/theme.css';
import './styles/tracking-pulse.css';

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
);
