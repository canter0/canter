"use client";

import { FormEvent, useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import type { MapMouseEvent } from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
import styles from "./weather.module.css";

type Place = { name: string; country: string; latitude: number; longitude: number };
type Forecast = {
  temperature_2m: number;
  apparent_temperature: number;
  relative_humidity_2m: number;
  wind_speed_10m: number;
  weather_code: number;
  daily: {
    time: string[];
    temperature_2m_max: number[];
    temperature_2m_min: number[];
    weather_code: number[];
  };
};

const cities: Place[] = [
  { name: "Toronto", country: "Canada", latitude: 43.6532, longitude: -79.3832 },
  { name: "Vancouver", country: "Canada", latitude: 49.2827, longitude: -123.1207 },
  { name: "London", country: "United Kingdom", latitude: 51.5072, longitude: -0.1276 },
  { name: "Tokyo", country: "Japan", latitude: 35.6762, longitude: 139.6503 },
];

function condition(code: number) {
  if (code === 0) return "Clear";
  if (code <= 3) return "Cloudy";
  if (code <= 48) return "Low visibility";
  if (code <= 67) return "Rain";
  if (code <= 77) return "Snow";
  if (code <= 82) return "Showers";
  if (code <= 86) return "Snow showers";
  return "Thunderstorms";
}

export function WeatherMap() {
  const mapNode = useRef<HTMLDivElement>(null);
  const map = useRef<import("maplibre-gl").Map | null>(null);
  const [place, setPlace] = useState(cities[0]);
  const [forecast, setForecast] = useState<Forecast | null>(null);
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const loadForecast = useCallback(async (next: Place) => {
    setPlace(next);
    setLoading(true);
    setError("");
    try {
      const params = new URLSearchParams({
        latitude: String(next.latitude),
        longitude: String(next.longitude),
        current: "temperature_2m,apparent_temperature,relative_humidity_2m,wind_speed_10m,weather_code",
        daily: "weather_code,temperature_2m_max,temperature_2m_min",
        forecast_days: "6",
        timezone: "auto",
      });
      const response = await fetch(`https://api.open-meteo.com/v1/forecast?${params}`);
      if (!response.ok) throw new Error();
      const data = await response.json();
      setForecast({ ...data.current, daily: data.daily });
    } catch {
      setError("Conditions are unavailable right now.");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    const timer = window.setTimeout(() => void loadForecast(cities[0]), 0);
    return () => window.clearTimeout(timer);
  }, [loadForecast]);

  useEffect(() => {
    if (!mapNode.current || map.current) return;
    let disposed = false;
    import("maplibre-gl").then((maplibregl) => {
      if (disposed || !mapNode.current) return;
      const instance = new maplibregl.Map({
        container: mapNode.current,
        style: "https://tiles.openfreemap.org/styles/positron",
        center: [cities[0].longitude, cities[0].latitude],
        zoom: 4,
        attributionControl: false,
      });
      instance.addControl(new maplibregl.NavigationControl({ showCompass: false }), "bottom-right");
      instance.addControl(new maplibregl.AttributionControl({ compact: true }), "bottom-left");
      instance.on("click", ({ lngLat }: MapMouseEvent) => {
        void loadForecast({
          name: "Pinned location",
          country: `${lngLat.lat.toFixed(2)}°, ${lngLat.lng.toFixed(2)}°`,
          latitude: lngLat.lat,
          longitude: lngLat.lng,
        });
      });
      map.current = instance;
    });
    return () => {
      disposed = true;
      map.current?.remove();
      map.current = null;
    };
  }, [loadForecast]);

  const selectPlace = useCallback((next: Place) => {
    void loadForecast(next);
    map.current?.flyTo({ center: [next.longitude, next.latitude], zoom: 6, essential: true });
  }, [loadForecast]);

  async function search(event: FormEvent) {
    event.preventDefault();
    if (!query.trim()) return;
    setLoading(true);
    setError("");
    try {
      const response = await fetch(
        `https://geocoding-api.open-meteo.com/v1/search?name=${encodeURIComponent(query)}&count=1&language=en&format=json`,
      );
      const data = await response.json();
      if (!data.results?.[0]) throw new Error();
      const result = data.results[0];
      selectPlace({
        name: result.name,
        country: result.country,
        latitude: result.latitude,
        longitude: result.longitude,
      });
      setQuery("");
    } catch {
      setError("Location not found. Try a nearby city.");
      setLoading(false);
    }
  }

  return (
    <main className={styles.shell}>
      <div ref={mapNode} className={styles.map} aria-label="Interactive world map. Select any point for its weather." />
      <div className={styles.tint} aria-hidden="true" />

      <header className={styles.header}>
        <Link className={styles.brand} href="/" aria-label="Canter home">canter</Link>
        <form className={styles.search} onSubmit={search}>
          <label htmlFor="weather-search">Find a city</label>
          <input
            id="weather-search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search anywhere"
          />
          <button type="submit">Search</button>
        </form>
        <span className={styles.live}><i /> Live weather</span>
      </header>

      <section className={styles.panel} aria-live="polite">
        <div className={styles.location}>
          <div>
            <p>Current conditions</p>
            <h1>{place.name}</h1>
            <span>{place.country}</span>
          </div>
          <b>{forecast ? condition(forecast.weather_code) : "—"}</b>
        </div>

        {loading ? (
          <div className={styles.loading}>Updating forecast…</div>
        ) : error ? (
          <p className={styles.error}>{error}</p>
        ) : forecast ? (
          <>
            <div className={styles.temperature}>
              <strong>{Math.round(forecast.temperature_2m)}°</strong>
              <span>Feels like {Math.round(forecast.apparent_temperature)}°</span>
            </div>
            <dl className={styles.metrics}>
              <div><dt>Wind</dt><dd>{Math.round(forecast.wind_speed_10m)} km/h</dd></div>
              <div><dt>Humidity</dt><dd>{forecast.relative_humidity_2m}%</dd></div>
              <div><dt>Latitude</dt><dd>{Math.abs(place.latitude).toFixed(1)}° {place.latitude >= 0 ? "N" : "S"}</dd></div>
            </dl>
            <div className={styles.days}>
              {forecast.daily.time.slice(1).map((day, index) => (
                <div key={day}>
                  <span>{new Intl.DateTimeFormat("en", { weekday: "short" }).format(new Date(`${day}T12:00:00`))}</span>
                  <small>{condition(forecast.daily.weather_code[index + 1])}</small>
                  <b>{Math.round(forecast.daily.temperature_2m_max[index + 1])}°</b>
                  <em>{Math.round(forecast.daily.temperature_2m_min[index + 1])}°</em>
                </div>
              ))}
            </div>
          </>
        ) : null}
      </section>

      <nav className={styles.cities} aria-label="Popular cities">
        {cities.map((city) => (
          <button
            type="button"
            key={city.name}
            className={place.name === city.name ? styles.active : undefined}
            onClick={() => selectPlace(city)}
          >
            {city.name}
          </button>
        ))}
      </nav>

      <p className={styles.hint}>Select anywhere on the map for local conditions</p>
    </main>
  );
}
