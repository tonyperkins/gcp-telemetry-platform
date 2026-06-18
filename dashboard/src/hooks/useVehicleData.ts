import { useEffect, useRef, useState } from 'react';
import { HealthStatus, Vehicle } from '../types/vehicle';

const API_BASE = import.meta.env.VITE_API_BASE_URL ?? '';
const POLL_INTERVAL_MS = 30_000;

interface VehicleDataState {
  vehicles: Vehicle[];
  health: HealthStatus | null;
  loading: boolean;
  error: string | null;
  lastUpdated: Date | null;
  isStale: boolean;
}

interface SimulationFlags {
  simulateMetroFailure?: boolean;
  simulateLatency?: boolean;
  simulateErrors?: boolean;
  recordRequest?: (responseTimeMs: number, payloadBytes: number, error?: string) => void;
}

/**
 * Polls /api/vehicles/current and /api/health every 30 seconds.
 *
 * Graceful degradation: if a poll fails, the hook retains the last
 * known data and sets isStale=true so the UI can indicate staleness
 * without wiping the map. A complete blank map is worse UX than a
 * slightly stale one.
 */
export function useVehicleData(isPaused = false, simulations: SimulationFlags = {}): VehicleDataState {
  const [state, setState] = useState<VehicleDataState>({
    vehicles: [],
    health: null,
    loading: true,
    error: null,
    lastUpdated: null,
    isStale: false,
  });

  const consecutiveErrors = useRef(0);

  const fetchData = async () => {
    const startTime = Date.now();
    try {
      if (simulations.simulateLatency) {
        await new Promise(resolve => setTimeout(resolve, 2500));
      }

      if (simulations.simulateErrors && Math.random() < 0.2) {
        throw new Error('Simulated error (20% error rate)');
      }

      const [vehicleRes, healthRes] = await Promise.all([
        fetch(`${API_BASE}/api/vehicles/current`),
        fetch(`${API_BASE}/api/health`),
      ]);

      if (!vehicleRes.ok) {
        throw new Error(`Vehicles API returned ${vehicleRes.status}`);
      }

      const vehicleText = await vehicleRes.text();
      const responseTimeMs = Date.now() - startTime;
      let rawVehicles: Vehicle[] = JSON.parse(vehicleText);
      let health: HealthStatus | null = healthRes.ok ? await healthRes.json() : null;
      simulations.recordRequest?.(responseTimeMs, vehicleText.length);

      // Final client-side deduplication pass (Safety Measure)
      const dedupedMap = new Map<string, Vehicle>();
      rawVehicles.forEach(v => {
        // We use vehicleId if available, otherwise fallback to id (record id)
        const key = v.vehicleId || v.id;
        // Keep the one with the latest ingestedAt if multiple exist in payload
        if (!dedupedMap.has(key) || new Date(v.ingestedAt) > new Date(dedupedMap.get(key)!.ingestedAt)) {
          dedupedMap.set(key, v);
        }
      });
      let vehicles = Array.from(dedupedMap.values());

      if (simulations.simulateMetroFailure) {
        vehicles = vehicles.filter(v => v.source !== 'metro');
        if (health) {
          health = {
            ...health,
            status: 'unhealthy',
            sources: {
              ...health.sources,
              metro: {
                status: 'unhealthy',
                lastIngest: health.sources.metro.lastIngest,
                vehicleCount: 0,
              },
            },
          };
        }
      }

      consecutiveErrors.current = 0;

      if (vehicles.length > 0) {
        (window as any).VEHICLE_LOG = vehicles;
        console.log(`[DATA] Fetched ${vehicles.length} vehicles`);
        if (consecutiveErrors.current === 0) {
          console.table(vehicles.slice(0, 5).map(v => ({ id: v.id, vId: v.vehicleId, lat: v.latitude, lon: v.longitude })));
        }
      }

      setState(prev => ({
        ...prev,
        vehicles,
        health,
        loading: false,
        error: null,
        lastUpdated: new Date(),
        isStale: false,
      }));
    } catch (err) {
      consecutiveErrors.current += 1;
      const responseTimeMs = Date.now() - startTime;
      simulations.recordRequest?.(responseTimeMs, 0, err instanceof Error ? err.message : 'Unknown error');

      setState(prev => ({
        ...prev,
        loading: false,
        error: err instanceof Error ? err.message : 'Unknown error',
        isStale: true,
        // Retain previous vehicles — map keeps showing last known positions
      }));
    }
  };

  useEffect(() => {
    if (isPaused) return;
    
    fetchData();
    const interval = setInterval(fetchData, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isPaused, simulations.simulateMetroFailure, simulations.simulateLatency, simulations.simulateErrors]);

  return state;
}
