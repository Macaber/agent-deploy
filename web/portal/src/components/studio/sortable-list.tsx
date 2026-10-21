import type { ReactNode } from 'react';
import {
  closestCenter,
  DndContext,
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core';
import { restrictToVerticalAxis } from '@dnd-kit/modifiers';
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { GripVertical } from 'lucide-react';
import { cn } from '@/lib/utils';

interface SortableListProps<T extends { id: string }> {
  items: T[];
  onReorder: (activeId: string, overId: string) => void;
  children: (item: T, index: number) => ReactNode;
  emptyLabel: string;
  ariaLabel: string;
}

export function SortableList<T extends { id: string }>({
  items,
  onReorder,
  children,
  emptyLabel,
  ariaLabel,
}: SortableListProps<T>) {
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  const handleDragEnd = (event: DragEndEvent) => {
    const { active, over } = event;
    if (!over) return;
    onReorder(String(active.id), String(over.id));
  };

  if (items.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-border bg-muted/15 px-3 py-4 text-center text-[11px] text-muted-foreground">
        {emptyLabel}
      </div>
    );
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      modifiers={[restrictToVerticalAxis]}
      onDragEnd={handleDragEnd}
      accessibility={{
        screenReaderInstructions: {
          draggable: '按空格键或回车拾起，使用方向键移动，再次按空格键放下，按 Esc 取消。',
        },
      }}
    >
      <SortableContext items={items.map((item) => item.id)} strategy={verticalListSortingStrategy}>
        <div className="flex flex-col gap-2" role="list" aria-label={ariaLabel}>
          {items.map((item, index) => (
            <SortableRow key={item.id} id={item.id} index={index}>
              {children(item, index)}
            </SortableRow>
          ))}
        </div>
      </SortableContext>
    </DndContext>
  );
}

function SortableRow({
  id,
  index,
  children,
}: {
  id: string;
  index: number;
  children: ReactNode;
}) {
  const {
    attributes,
    listeners,
    setNodeRef,
    setActivatorNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id });

  return (
    <div
      ref={setNodeRef}
      style={{
        transform: CSS.Transform.toString(transform),
        transition,
      }}
      className={cn(
        'group relative flex items-start gap-2 rounded-lg border border-border bg-background p-2 shadow-xs transition-[border-color,box-shadow] hover:border-ring/60',
        isDragging && 'z-20 border-ring opacity-70 shadow-lg',
      )}
      role="listitem"
      aria-posinset={index + 1}
    >
      <button
        ref={setActivatorNodeRef}
        type="button"
        {...attributes}
        {...listeners}
        aria-label="拖拽调整顺序"
        className="drag-handle mt-0.5 grid size-7 shrink-0 place-items-center rounded text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40"
      >
        <GripVertical className="size-3.5" />
      </button>
      <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center">{children}</div>
    </div>
  );
}
