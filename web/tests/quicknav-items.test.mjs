import test from 'node:test';
import assert from 'node:assert/strict';
import {quicknavItems, quicknavFooterItems} from '../src/lib/quicknav-items.ts';

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
test('quicknav previews recent conversations on home and describes products', () => {
  const items = quicknavItems([{id:'wrens',name:'Wrens',description:'Persistent agents.',enabled:true,alwaysOn:true}]);
  assert.equal(items[0].preview, 'recent');
  assert.equal(items[1].preview, 'product');
  assert.equal(items[1].description, 'Persistent agents.');
  assert.equal(items[2].preview, undefined);
});
test('quicknav footer pins account, settings, and customize in order', () => {
  assert.deepEqual(quicknavFooterItems.map(item => item.href), ['/app/settings','/app/customize']);
});
