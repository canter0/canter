"use client";

import { useEffect, useRef } from "react";

// Adapted from React Bits Dither by David Haz; see licenses/react-bits.txt.
// https://reactbits.dev/backgrounds/dither
// A single WebGL pass avoids a 3D runtime for this decorative background.
const vertexSource = `#version 300 es
in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }
`;
const fragmentSource = `#version 300 es
precision highp float;
uniform vec2 resolution;
uniform float time;
uniform vec2 mouse;
out vec4 outputColor;
vec4 mod289(vec4 x) { return x - floor(x / 289.0) * 289.0; }
vec4 permute(vec4 x) { return mod289(((x * 34.0) + 1.0) * x); }
vec4 taylorInvSqrt(vec4 r) { return 1.79284291400159 - 0.85373472095314 * r; }
vec2 fade(vec2 t) { return t*t*t*(t*(t*6.0-15.0)+10.0); }
float cnoise(vec2 P) {
  vec4 Pi = floor(P.xyxy) + vec4(0.0,0.0,1.0,1.0);
  vec4 Pf = fract(P.xyxy) - vec4(0.0,0.0,1.0,1.0);
  Pi = mod289(Pi);
  vec4 ix = Pi.xzxz;
  vec4 iy = Pi.yyww;
  vec4 fx = Pf.xzxz;
  vec4 fy = Pf.yyww;
  vec4 i = permute(permute(ix) + iy);
  vec4 gx = fract(i / 41.0) * 2.0 - 1.0;
  vec4 gy = abs(gx) - 0.5;
  gx -= floor(gx + 0.5);
  vec2 g00 = vec2(gx.x, gy.x);
  vec2 g10 = vec2(gx.y, gy.y);
  vec2 g01 = vec2(gx.z, gy.z);
  vec2 g11 = vec2(gx.w, gy.w);
  vec4 norm = taylorInvSqrt(vec4(dot(g00,g00), dot(g01,g01), dot(g10,g10), dot(g11,g11)));
  g00 *= norm.x; g01 *= norm.y; g10 *= norm.z; g11 *= norm.w;
  float n00 = dot(g00, vec2(fx.x, fy.x));
  float n10 = dot(g10, vec2(fx.y, fy.y));
  float n01 = dot(g01, vec2(fx.z, fy.z));
  float n11 = dot(g11, vec2(fx.w, fy.w));
  vec2 n_x = mix(vec2(n00, n01), vec2(n10, n11), fade(Pf.xy).x);
  return 2.3 * mix(n_x.x, n_x.y, fade(Pf.xy).y);
}
float fbm(vec2 p) {
  float value = 0.0;
  float amplitude = 1.0;
  for (int i = 0; i < 4; i++) {
    value += amplitude * abs(cnoise(p));
    p *= 3.0;
    amplitude *= 0.3;
  }
  return value;
}
const float bayer[64] = float[64](
  0.,48.,12.,60.,3.,51.,15.,63.,32.,16.,44.,28.,35.,19.,47.,31.,
  8.,56.,4.,52.,11.,59.,7.,55.,40.,24.,36.,20.,43.,27.,39.,23.,
  2.,50.,14.,62.,1.,49.,13.,61.,34.,18.,46.,30.,33.,17.,45.,29.,
  10.,58.,6.,54.,9.,57.,5.,53.,42.,26.,38.,22.,41.,25.,37.,21.
);
void main() {
  vec2 pixel = floor(gl_FragCoord.xy / 2.0);
  vec2 uv = pixel * 2.0 / resolution - 0.5;
  uv.x *= resolution.x / resolution.y;
  float f = fbm(uv + fbm(uv - time * 0.05));
  vec2 mouseUV = (mouse / resolution - 0.5) * vec2(1.0, -1.0);
  mouseUV.x *= resolution.x / resolution.y;
  f -= 0.5 * (1.0 - smoothstep(0.0, 1.0, length(uv - mouseUV)));
  float threshold = bayer[int(mod(pixel.y, 8.0)) * 8 + int(mod(pixel.x, 8.0))] / 64.0 - 0.25;
  float ink = floor(clamp(f + threshold / 3.0, 0.0, 1.0) * 3.0 + 0.5) / 3.0;
  vec3 paper = vec3(249.0, 250.0, 246.0) / 255.0;
  vec3 green = vec3(0.07450980392156863, 1.0, 0.0);
  outputColor = vec4(mix(paper, green, ink), 1.0);
}
`;

export function DitherBackground({ className }: { className?: string }) {
  const canvasRef = useRef<HTMLCanvasElement>(null);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const gl = canvas.getContext("webgl2", { alpha: false, antialias: false, powerPreference: "low-power" });
    if (!gl) return;

    const shaders: WebGLShader[] = [];
    function compile(type: number, source: string) {
      const shader = gl!.createShader(type);
      if (!shader) return null;
      shaders.push(shader);
      gl!.shaderSource(shader, source);
      gl!.compileShader(shader);
      return gl!.getShaderParameter(shader, gl!.COMPILE_STATUS) ? shader : null;
    }
    const vertex = compile(gl.VERTEX_SHADER, vertexSource);
    const fragment = compile(gl.FRAGMENT_SHADER, fragmentSource);
    const program = gl.createProgram();
    const buffer = gl.createBuffer();
    const dispose = () => {
      shaders.forEach((shader) => gl.deleteShader(shader));
      gl.deleteBuffer(buffer);
      gl.deleteProgram(program);
    };
    if (!vertex || !fragment || !program || !buffer) { dispose(); return; }
    gl.attachShader(program, vertex);
    gl.attachShader(program, fragment);
    gl.linkProgram(program);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) { dispose(); return; }
    gl.useProgram(program);
    gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
    const position = gl.getAttribLocation(program, "position");
    gl.enableVertexAttribArray(position);
    gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
    const resolution = gl.getUniformLocation(program, "resolution");
    const time = gl.getUniformLocation(program, "time");
    const mouse = gl.getUniformLocation(program, "mouse");
    gl.uniform2f(mouse, -10000, -10000);

    const motion = window.matchMedia("(prefers-reduced-motion: reduce)");
    const frame = canvas.parentElement!.parentElement!;
    let raf = 0;
    let lastDraw = 0;
    let elapsed = 18;
    let visible = true;
    let lost = false;

    function draw() {
      if (lost) return;
      gl!.uniform1f(time, elapsed);
      gl!.drawArrays(gl!.TRIANGLES, 0, 3);
      canvas!.style.opacity = "1";
    }
    function animate(now: number) {
      if (now - lastDraw >= 1000 / 30) {
        elapsed += Math.min((now - lastDraw) / 1000, 0.1);
        lastDraw = now;
        draw();
      }
      raf = requestAnimationFrame(animate);
    }
    function syncAnimation() {
      cancelAnimationFrame(raf);
      if (lost) return;
      draw();
      if (!motion.matches && visible && !document.hidden) {
        lastDraw = performance.now();
        raf = requestAnimationFrame(animate);
      }
    }
    function resize() {
      // CSS pixels match the reference's dpr=1 and bound GPU cost.
      canvas!.width = Math.max(1, Math.round(canvas!.clientWidth));
      canvas!.height = Math.max(1, Math.round(canvas!.clientHeight));
      gl!.viewport(0, 0, canvas!.width, canvas!.height);
      gl!.uniform2f(resolution, canvas!.width, canvas!.height);
      draw();
    }
    function pointerMove(event: PointerEvent) {
      if (motion.matches || event.pointerType === "touch") return;
      const bounds = canvas!.getBoundingClientRect();
      gl!.uniform2f(mouse, event.clientX - bounds.left, event.clientY - bounds.top);
    }
    function pointerLeave() { gl!.uniform2f(mouse, -10000, -10000); }
    function contextLost() {
      lost = true;
      cancelAnimationFrame(raf);
      canvas!.style.opacity = "0";
    }
    const resizeObserver = new ResizeObserver(resize);
    const intersectionObserver = new IntersectionObserver(([entry]) => {
      visible = entry.isIntersecting;
      syncAnimation();
    });
    resizeObserver.observe(canvas);
    intersectionObserver.observe(canvas);
    frame.addEventListener("pointermove", pointerMove, { passive: true });
    frame.addEventListener("pointerleave", pointerLeave);
    canvas.addEventListener("webglcontextlost", contextLost);
    motion.addEventListener("change", syncAnimation);
    document.addEventListener("visibilitychange", syncAnimation);
    resize();
    syncAnimation();

    return () => {
      cancelAnimationFrame(raf);
      resizeObserver.disconnect();
      intersectionObserver.disconnect();
      frame.removeEventListener("pointermove", pointerMove);
      frame.removeEventListener("pointerleave", pointerLeave);
      canvas.removeEventListener("webglcontextlost", contextLost);
      motion.removeEventListener("change", syncAnimation);
      document.removeEventListener("visibilitychange", syncAnimation);
      dispose();
    };
  }, []);

  return <div className={className} aria-hidden="true"><canvas ref={canvasRef} style={{ opacity: 0 }} /></div>;
}
