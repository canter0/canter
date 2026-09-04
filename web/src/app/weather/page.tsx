import type { Metadata } from "next";
import { WeatherMap } from "./weather-map";

export const metadata: Metadata = {
  title: "Weather map",
  description: "Live conditions and a six-day forecast for anywhere in the world.",
};

export default function WeatherPage() {
  return <WeatherMap />;
}
