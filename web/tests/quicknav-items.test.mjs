import test from 'node:test';
import assert from 'node:assert/strict';
import {quicknavItems} from '../src/lib/quicknav-items.ts';

const catalog = [
  {id:'wrens',name:'Wrens',description:'',enabled:true,alwaysOn:true},
  {id:'sandboxes',name:'Sandboxes',description:'',enabled:false,alwaysOn:false},
];
test('quicknav lists home, enabled products in order, then products', () => {
  const items = quicknavItems(catalog);
  assert.deepEqual(items.map(item => item.href), ['/app','/app/wrens','/app/products']);
  assert.equal(items[1].wren, true);
});
test('enabling a product adds its icon without reload', () => {
  const items = quicknavItems(catalog.map(product => product.id==='sandboxes' ? {...product, enabled:true} : product));
  assert.deepEqual(items.map(item => item.href), ['/app','/app/wrens','/app/sandboxes','/app/products']);
  assert.equal(items[2].icon, 'terminal');
});
test('unknown or missing products never appear', () => {
  const items = quicknavItems([...catalog, {id:'future',name:'Future',description:'',enabled:true,alwaysOn:false}]);
  assert.deepEqual(items.map(item => item.href), ['/app','/app/wrens','/app/products']);
  assert.deepEqual(quicknavItems(undefined).map(item => item.href), ['/app','/app/products']);
});
