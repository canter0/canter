import test from 'node:test';
import assert from 'node:assert/strict';
import { movingThroughHoverTriangle } from '../src/lib/hover-intent.ts';

const card = {left:230,right:454,top:160,bottom:270};
const origin = {x:90,y:65};
function follow(points, target=card, start=origin, side='right') {
  let previous=start;
  for (const point of points) {
    assert.equal(movingThroughHoverTriangle(point,previous,start,target,side),true, JSON.stringify({point,previous,start,side}));
    previous=point;
  }
}
test('a diagonal path crossing other model rows keeps the original card',()=>{
  follow([{x:110,y:86},{x:150,y:126},{x:190,y:170},{x:228,y:210}]);
});
test('slow pointer travel remains protected without requiring a speed threshold',()=>{
  follow(Array.from({length:100},(_,i)=>({x:91+i,y:66+(i*1.1)})));
});
test('the safe corridor works for a card opening on the left',()=>{
  const mirror=point=>({x:500-point.x,y:point.y});
  follow([{x:110,y:86},{x:150,y:126},{x:190,y:170},{x:228,y:210}].map(mirror),{left:46,right:270,top:160,bottom:270},mirror(origin),'left');
});
test('vertical browsing, reversing direction and exiting the triangle are immediate',()=>{
  assert.equal(movingThroughHoverTriangle({x:90,y:100},origin,origin,card,'right'),false);
  assert.equal(movingThroughHoverTriangle({x:130,y:130},{x:160,y:120},origin,card,'right'),false);
  assert.equal(movingThroughHoverTriangle({x:190,y:280},{x:180,y:200},origin,card,'right'),false);
});
test('near-horizontal paths across the gap and upward diagonals are protected',()=>{
  follow([{x:140,y:200},{x:190,y:200},{x:228,y:200}],card,{x:90,y:200});
  follow([{x:130,y:275},{x:180,y:235},{x:228,y:200}],card,{x:90,y:310});
});

test('a tall grouped menu keeps the preview when aiming through the top of the card',()=>{
  const target={left:636,right:916,top:577,bottom:672};
  const start={x:520,y:332};
  follow(Array.from({length:20},(_,i)=>({x:520+(i+1)*17,y:332+(i+1)*14.4})),target,start);
  const mirror=point=>({x:1280-point.x,y:point.y});
  follow(Array.from({length:20},(_,i)=>mirror({x:520+(i+1)*17,y:332+(i+1)*14.4})),{left:364,right:644,top:577,bottom:672},mirror(start),'left');
});

test('aiming through the bottom of a card above the row stays protected',()=>{
  follow(Array.from({length:20},(_,i)=>({x:520+(i+1)*17,y:620-(i+1)*14.4})),{left:636,right:916,top:280,bottom:375},{x:520,y:620});
});

test('small pointer jitter during a diagonal does not switch rows',()=>{
  follow([{x:130,y:110},{x:160,y:150},{x:160,y:151},{x:159,y:152},{x:190,y:200},{x:228,y:230}]);
});
