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
