"use client";

import { useEffect, useRef } from "react";
import styles from "./onboarding-prism.module.css";

/* The Prism shader below is adapted from React Bits by David Haz.
     Source: https://github.com/DavidHDev/react-bits/blob/main/src/ts-default/Backgrounds/Prism/Prism.tsx

     MIT + Commons Clause License Condition v1.0
     Copyright (c) 2026 David Haz
     Permission is hereby granted, free of charge, to any person obtaining a copy
     of this software and associated documentation files (the "Software"), to deal
     in the Software without restriction, including without limitation the rights
     to use, copy, modify, merge, publish, and distribute the Software as part of
     an application, website, or product, subject to the following conditions:
     The above copyright notice and this permission notice shall be included in all
     copies or substantial portions of the Software.
     Commons Clause Restriction: You may use this Software, including for any
     commercial purpose, so long as you do not sell, sublicense, or redistribute
     the components themselves-whether alone, in a bundle, or as a ported version.
     THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
     IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
     FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
     AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
     LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
     OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
  */

const vertex = "attribute vec2 position; void main(){gl_Position=vec4(position,0.,1.);}";
const fragment=`precision highp float;
      uniform vec2 resolution; uniform float time;
      float random(vec2 p){return fract(sin(dot(p,vec2(12.9898,78.233)))*43758.5453);}
      float pyramid(vec3 p){vec3 q=abs(p)*vec3(.363636,.285714,.363636);return max((q.x+q.y+q.z-1.)*1.5877,-p.y);}
      void main(){
        vec2 f=(gl_FragCoord.xy-.5*resolution-vec2(resolution.x*.18,0.))/(resolution.y*.36);
        float t=.35+time*.06; mat2 wob=mat2(cos(t),cos(t+33.),cos(t+11.),cos(t));
        float z=5.; vec4 o=vec4(0.);
        for(int i=0;i<100;i++){vec3 p=vec3(f,z);p.xz=p.xz*wob;vec3 q=p;q.y+=.875;float d=.1+.2*abs(pyramid(q));z-=d;o+=(sin((p.y+z)*2.+vec4(0.,1.,2.,3.))+1.)/d;}
        vec4 v=clamp(o*o/100000.,0.,8.);vec4 e=exp(2.*v);o=(e-1.)/(e+1.);
        vec3 col=clamp(o.rgb+(random(gl_FragCoord.xy)-.5)*.08,0.,1.);
        float luminance=dot(col,vec3(.2126,.7152,.0722));col=clamp(mix(vec3(luminance),col,1.5),0.,1.);
        gl_FragColor=vec4(col,1.);
      }`;

export function OnboardingPrism({ leaving, reducedMotion }: { leaving: boolean; reducedMotion: boolean }) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const gl = canvas.getContext("webgl", { alpha: false, antialias: false });
    if (!gl) return;
    function compile(type: number, source: string) {
      const shader = gl!.createShader(type);
      if (!shader) return null;
      gl!.shaderSource(shader, source);
      gl!.compileShader(shader);
      if (!gl!.getShaderParameter(shader, gl!.COMPILE_STATUS)) { gl!.deleteShader(shader); return null; }
      return shader;
    }
    const v = compile(gl.VERTEX_SHADER, vertex), f = compile(gl.FRAGMENT_SHADER, fragment);
    if (!v || !f) { if (v) gl.deleteShader(v); if (f) gl.deleteShader(f); return; }
    const program = gl.createProgram();
    if (!program) { gl.deleteShader(v); gl.deleteShader(f); return; }
    gl.attachShader(program, v); gl.attachShader(program, f); gl.linkProgram(program);
    gl.deleteShader(v); gl.deleteShader(f);
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) { gl.deleteProgram(program); return; }
    gl.useProgram(program);
    const buffer = gl.createBuffer();
    gl.bindBuffer(gl.ARRAY_BUFFER, buffer);
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
    const position = gl.getAttribLocation(program, "position");
    gl.enableVertexAttribArray(position); gl.vertexAttribPointer(position, 2, gl.FLOAT, false, 0, 0);
    const resolution = gl.getUniformLocation(program, "resolution"), clock = gl.getUniformLocation(program, "time");
    const started = performance.now();
    let frame = 0, previous = 0;
    function draw(time = performance.now()) {
      if (!canvas || !gl || gl.isContextLost()) return;
      const box = canvas.getBoundingClientRect();
      const width = Math.max(1, Math.round(box.width * .65)), height = Math.max(1, Math.round(box.height * .65));
      if (canvas.width !== width || canvas.height !== height) { canvas.width = width; canvas.height = height; gl.viewport(0, 0, width, height); }
      gl.uniform2f(resolution, width, height);
      gl.uniform1f(clock, reducedMotion ? 2 : Math.min(6, (time - started) / 1000));
      gl.drawArrays(gl.TRIANGLES, 0, 3);
      canvas.dataset.ready = "true";
    }
    function tick(time: number) {
      if (time - previous > 33 && !document.hidden) { draw(time); previous = time; }
      if (time - started < 6000) frame = requestAnimationFrame(tick);
    }
    const observer = new ResizeObserver(() => draw());
    observer.observe(canvas);
    draw();
    if (!reducedMotion) frame = requestAnimationFrame(tick);
    return () => { observer.disconnect(); cancelAnimationFrame(frame); gl.deleteBuffer(buffer); gl.deleteProgram(program); };
  }, [reducedMotion]);
  return <div className={styles.prism} data-leaving={leaving} aria-hidden="true"><canvas ref={canvasRef} /><div className={styles.shade} /></div>;
}
