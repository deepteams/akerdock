import { TestBed } from '@angular/core/testing';

import { ActionsMenuComponent, type ActionItem } from './actions-menu.component';

const ITEMS: ActionItem[] = [
  { id: 'recreate', label: 'Recreate (apply config)', icon: 'settings-2' },
  { id: 'restart', label: 'Restart', icon: 'refresh-cw' },
  { id: 'stop', label: 'Stop', icon: 'square', danger: true, disabled: true },
];

function mount() {
  const fixture = TestBed.createComponent(ActionsMenuComponent);
  fixture.componentRef.setInput('items', ITEMS);
  fixture.detectChanges();
  return fixture;
}

describe('actions menu', () => {
  it('stays closed until the trigger is clicked', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;

    expect(host.querySelector('[role="menu"]')).toBeNull();

    host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="menu"]')).not.toBeNull();
    expect(host.querySelectorAll('[role="menuitem"]').length).toBe(ITEMS.length);
  });

  // The document listener must not close what the very same click opened.
  it('survives the click that opened it bubbling to the document', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;

    host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="menu"]')).not.toBeNull();
  });

  it('emits the picked id and closes', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;
    const picked: string[] = [];
    fixture.componentInstance.selected.subscribe((id: string) => picked.push(id));

    host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();
    const items = host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]');
    items[0].click();
    fixture.detectChanges();

    expect(picked).toEqual(['recreate']);
    expect(host.querySelector('[role="menu"]')).toBeNull();
  });

  it('renders a disabled entry as unclickable', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;

    host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    const items = host.querySelectorAll<HTMLButtonElement>('[role="menuitem"]');
    expect(items[2].disabled).toBeTrue();
  });

  it('disables the whole menu when the page is busy', () => {
    const fixture = mount();
    fixture.componentRef.setInput('disabled', true);
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('button').disabled).toBeTrue();
  });

  it('closes when a click lands outside', () => {
    const fixture = mount();
    const host: HTMLElement = fixture.nativeElement;

    host.querySelector('button')!.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    fixture.detectChanges();

    expect(host.querySelector('[role="menu"]')).toBeNull();
  });
});
